// Copyright 2016 Amazon.com, Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may not
// use this file except in compliance with the License. A copy of the
// License is located at
//
// http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND,
// either express or implied. See the License for the specific language governing
// permissions and limitations under the License.

// Package processor defines the document processing unit interface
package processor

import (
	"fmt"
	"io/ioutil"
	"os"
	"sync"
	"time"

	"path/filepath"

	"github.com/aws/amazon-ssm-agent/agent/appconfig"
	"github.com/aws/amazon-ssm-agent/agent/context"
	"github.com/aws/amazon-ssm-agent/agent/contracts"
	"github.com/aws/amazon-ssm-agent/agent/docmanager"
	"github.com/aws/amazon-ssm-agent/agent/docmanager/model"
	"github.com/aws/amazon-ssm-agent/agent/fileutil"
	"github.com/aws/amazon-ssm-agent/agent/framework/processor/executer"
	"github.com/aws/amazon-ssm-agent/agent/framework/processor/executer/basicexecuter"
	"github.com/aws/amazon-ssm-agent/agent/longrunning/manager"
	"github.com/aws/amazon-ssm-agent/agent/platform"
	"github.com/aws/amazon-ssm-agent/agent/rebooter"
	"github.com/aws/amazon-ssm-agent/agent/task"
	"github.com/aws/amazon-ssm-agent/agent/times"
)

type ExecuterCreator func(ctx context.T) executer.Executer

const (

	// hardstopTimeout is the time before the processor will be shutdown during a hardstop
	hardStopTimeout = time.Second * 4
)

type Processor interface {
	//Start activate the Processor and pick up the left over document in the last run, it returns a channel to caller to gather DocumentResult
	Start() (chan contracts.DocumentResult, error)
	//Stop the processor, save the current state to resume later
	Stop(stopType contracts.StopType)
	//submit to the pool a document in form of docState object, results will be streamed back from the central channel returned by Start()
	Submit(docState model.DocumentState)
	//cancel process the cancel document, with no return value since the command is already tracked in a different thread
	Cancel(docState model.DocumentState)
	//TODO do we need to implement CancelAll?
	//CancelAll()
}

type EngineProcessor struct {
	context           context.T
	executerCreator   ExecuterCreator
	sendCommandPool   task.Pool
	cancelCommandPool task.Pool
	//TODO this should be abstract as the Processor's domain
	supportedDocTypes []model.DocumentType
	resChan           chan contracts.DocumentResult
}

//TODO worker pool should be triggered in the Start() function
//supported document types indicate the domain of the documentes the Processor with run upon. There'll be race-conditions if there're multiple Processors in a certain domain.
func NewEngineProcessor(ctx context.T, commandWorkerLimit int, cancelWorkerLimit int, supportedDocs []model.DocumentType) *EngineProcessor {
	log := ctx.Log()
	// sendCommand and cancelCommand will be processed by separate worker pools
	// so we can define the number of workers per each
	cancelWaitDuration := 10000 * time.Millisecond
	clock := times.DefaultClock
	sendCommandTaskPool := task.NewPool(log, commandWorkerLimit, cancelWaitDuration, clock)
	cancelCommandTaskPool := task.NewPool(log, cancelWorkerLimit, cancelWaitDuration, clock)
	resChan := make(chan contracts.DocumentResult)
	executerCreator := func(ctx context.T) executer.Executer {
		return basicexecuter.NewBasicExecuter(ctx)
	}
	return &EngineProcessor{
		context:           ctx.With("[EngineProcessor]"),
		executerCreator:   executerCreator,
		sendCommandPool:   sendCommandTaskPool,
		cancelCommandPool: cancelCommandTaskPool,
		supportedDocTypes: supportedDocs,
		resChan:           resChan,
	}
}

func (p *EngineProcessor) Start() (resChan chan contracts.DocumentResult, err error) {
	context := p.context
	if context == nil {
		return nil, fmt.Errorf("EngineProcessor is not initialized")
	}
	log := context.Log()
	//process the older jobs from Current & Pending folder
	instanceID, err := platform.InstanceID()
	if err != nil {
		log.Errorf("no instanceID provided, %v", err)
		return
	}
	resChan = p.resChan
	//prioritie the ongoing document first
	p.processInProgressDocuments(instanceID)
	//deal with the pending jobs that haven't picked up by worker yet
	p.processPendingDocuments(instanceID)
	return
}

//Submit() is the public interface for sending run document request to processor
func (p *EngineProcessor) Submit(docState model.DocumentState) {
	log := p.context.Log()
	//queue up the pending document
	docmanager.PersistData(log, docState.DocumentInformation.DocumentID, docState.DocumentInformation.InstanceID, appconfig.DefaultLocationOfPending, docState)
	err := p.submit(&docState)
	if err != nil {
		log.Error("Document Submission failed", err)
		//move the fail-to-submit document to corrupt folder
		docmanager.MoveDocumentState(log, docState.DocumentInformation.DocumentID, docState.DocumentInformation.InstanceID, appconfig.DefaultLocationOfPending, appconfig.DefaultLocationOfCorrupt)
		return
	}
	return
}

func (p *EngineProcessor) submit(docState *model.DocumentState) error {
	log := p.context.Log()
	//TODO this is a hack, in future jobID should be managed by Processing engine itself, instead of inferring from job's internal field
	var jobID string
	if docState.IsAssociation() {
		jobID = docState.DocumentInformation.AssociationID
	} else {
		jobID = docState.DocumentInformation.MessageID
	}
	return p.sendCommandPool.Submit(log, jobID, func(cancelFlag task.CancelFlag) {
		processCommand(
			p.context,
			p.executerCreator,
			cancelFlag,
			p.resChan,
			docState)
	})

}

func (p *EngineProcessor) Cancel(docState model.DocumentState) {
	log := p.context.Log()
	//TODO this is a hack, in future jobID should be managed by Processing engine itself, instead of inferring from job's internal field
	var jobID string
	if docState.IsAssociation() {
		jobID = docState.DocumentInformation.AssociationID
	} else {
		jobID = docState.DocumentInformation.MessageID
	}
	//queue up the pending document
	docmanager.PersistData(log, docState.DocumentInformation.DocumentID, docState.DocumentInformation.InstanceID, appconfig.DefaultLocationOfPending, docState)
	err := p.cancelCommandPool.Submit(log, jobID, func(cancelFlag task.CancelFlag) {
		processCancelCommand(p.context, p.sendCommandPool, &docState)
	})
	if err != nil {
		log.Error("CancelCommand failed", err)
		return
	}
}

//Stop set the cancel flags of all the running jobs, which are to be captured by the command worker and shutdown gracefully
func (p *EngineProcessor) Stop(stopType contracts.StopType) {
	var waitTimeout time.Duration

	if stopType == contracts.StopTypeSoftStop {
		waitTimeout = time.Duration(p.context.AppConfig().Mds.StopTimeoutMillis) * time.Millisecond
	} else {
		waitTimeout = hardStopTimeout
	}

	var wg sync.WaitGroup

	// shutdown the send command pool in a separate go routine
	wg.Add(1)
	go func() {
		defer wg.Done()
		p.sendCommandPool.ShutdownAndWait(waitTimeout)
	}()

	// shutdown the cancel command pool in a separate go routine
	wg.Add(1)
	go func() {
		defer wg.Done()
		p.cancelCommandPool.ShutdownAndWait(waitTimeout)
	}()

	// wait for everything to shutdown
	wg.Wait()
	// close the receiver channel only after we're sure all the ongoing jobs are stopped and no sender is on this channel
	close(p.resChan)
}

//TODO remove the direct file dependency once we encapsulate docmanager package
func (p *EngineProcessor) processPendingDocuments(instanceID string) {
	log := p.context.Log()
	files := []os.FileInfo{}
	var err error

	//process older documents from PENDING folder
	pendingDocsLocation := docmanager.DocumentStateDir(instanceID, appconfig.DefaultLocationOfPending)

	if isDirectoryEmpty, _ := fileutil.IsDirEmpty(pendingDocsLocation); isDirectoryEmpty {
		log.Debugf("No documents to process from %v", pendingDocsLocation)
		return
	}

	//get all pending messages
	if files, err = fileutil.ReadDir(pendingDocsLocation); err != nil {
		log.Errorf("skipping reading pending documents from %v. unexpected error encountered - %v", pendingDocsLocation, err)
		return
	}

	//iterate through all pending messages
	for _, f := range files {
		log.Debugf("Processing an older document - %v", f.Name())
		//inspect document state
		docState := docmanager.GetDocumentInterimState(log, f.Name(), instanceID, appconfig.DefaultLocationOfPending)

		if p.isSupportedDocumentType(docState.DocumentType) {
			log.Debugf("processor processing pending document %v", docState.DocumentInformation.DocumentID)
			p.Submit(docState)
		}

	}
}

// ProcessInProgressDocuments processes InProgress documents that have already dequeued and entered job pool
func (p *EngineProcessor) processInProgressDocuments(instanceID string) {
	log := p.context.Log()
	config := p.context.AppConfig()
	var err error

	pendingDocsLocation := docmanager.DocumentStateDir(instanceID, appconfig.DefaultLocationOfCurrent)

	if isDirectoryEmpty, _ := fileutil.IsDirEmpty(pendingDocsLocation); isDirectoryEmpty {
		log.Debugf("no older document to process from %v", pendingDocsLocation)
		return

	}

	files := []os.FileInfo{}
	if files, err = ioutil.ReadDir(pendingDocsLocation); err != nil {
		log.Errorf("skipping reading inprogress document from %v. unexpected error encountered - %v", pendingDocsLocation, err)
		return
	}

	//iterate through all InProgress docs
	for _, f := range files {
		log.Debugf("processing previously unexecuted document - %v", f.Name())

		//inspect document state
		docState := docmanager.GetDocumentInterimState(log, f.Name(), instanceID, appconfig.DefaultLocationOfCurrent)

		retryLimit := config.Mds.CommandRetryLimit
		if docState.DocumentInformation.RunCount >= retryLimit {
			docmanager.MoveDocumentState(log, f.Name(), instanceID, appconfig.DefaultLocationOfCurrent, appconfig.DefaultLocationOfCorrupt)
			continue
		}

		// increment the command run count
		docState.DocumentInformation.RunCount++

		docmanager.PersistData(log, docState.DocumentInformation.DocumentID, instanceID, appconfig.DefaultLocationOfCurrent, docState)

		if p.isSupportedDocumentType(docState.DocumentType) {
			log.Debugf("processor processing in-progress document %v", docState.DocumentInformation.DocumentID)
			//Submit the work to Job Pool so that we don't block for processing of new messages
			if err := p.submit(&docState); err != nil {
				log.Errorf("failed to submit in progress document %v : %v", docState.DocumentInformation.DocumentID, err)
				docmanager.MoveDocumentState(log, f.Name(), instanceID, appconfig.DefaultLocationOfCurrent, appconfig.DefaultLocationOfCorrupt)
			}
		}
	}
}

func (p *EngineProcessor) isSupportedDocumentType(documentType model.DocumentType) bool {
	for _, d := range p.supportedDocTypes {
		if documentType == d {
			return true
		}
	}
	return false
}

func processCommand(context context.T, executerCreator ExecuterCreator, cancelFlag task.CancelFlag, resChan chan contracts.DocumentResult, docState *model.DocumentState) {
	log := context.Log()
	//persist the current running document
	docmanager.MoveDocumentState(log,
		docState.DocumentInformation.DocumentID,
		docState.DocumentInformation.InstanceID,
		appconfig.DefaultLocationOfPending,
		appconfig.DefaultLocationOfCurrent)
	log.Debug("Running executer...")
	documentID := docState.DocumentInformation.DocumentID
	instanceID := docState.DocumentInformation.InstanceID
	messageID := docState.DocumentInformation.MessageID
	e := executerCreator(context)
	docStore := executer.NewDocumentFileStore(context, instanceID, documentID, appconfig.DefaultLocationOfCurrent, docState)
	statusChan := e.Run(
		cancelFlag,
		&docStore,
	)
	// Listen for reboot
	isReboot := false
	for res := range statusChan {
		if res.LastPlugin == "" {
			log.Infof("sending document: %v complete response", documentID)
		} else {
			log.Infof("sending reply for plugin update: %v", res.LastPlugin)

		}
		handleCloudwatchPlugin(context, res.PluginResults, documentID)
		//hand off the message to Service
		resChan <- res
		isReboot = res.Status == contracts.ResultStatusSuccessAndReboot
	}
	//TODO since there's a bug in UpdatePlugin that returns InProgress even if the document is completed, we cannot use InProgress to judge here, we need to fix the bug by the time out-of-proc is done
	// Shutdown/reboot detection
	if isReboot {
		log.Infof("document %v requested reboot, need to resume", messageID)
		rebooter.RequestPendingReboot(context.Log())
		return
	}

	//persist : commands execution in completed folder (terminal state folder)
	log.Debugf("execution of %v is over. Moving interimState file from Current to Completed folder", messageID)

	docmanager.MoveDocumentState(log,
		documentID,
		instanceID,
		appconfig.DefaultLocationOfCurrent,
		appconfig.DefaultLocationOfCompleted)

}

//TODO CancelCommand is currently treated as a special type of Command by the Processor, but in general Cancel operation should be seen as a probe to existing commands
func processCancelCommand(context context.T, sendCommandPool task.Pool, docState *model.DocumentState) {

	log := context.Log()

	log.Debugf("Canceling job with id %v...", docState.CancelInformation.CancelMessageID)

	if found := sendCommandPool.Cancel(docState.CancelInformation.CancelMessageID); !found {
		log.Debugf("Job with id %v not found (possibly completed)", docState.CancelInformation.CancelMessageID)
		docState.CancelInformation.DebugInfo = fmt.Sprintf("Command %v couldn't be cancelled", docState.CancelInformation.CancelCommandID)
		docState.DocumentInformation.DocumentStatus = contracts.ResultStatusFailed
	} else {
		docState.CancelInformation.DebugInfo = fmt.Sprintf("Command %v cancelled", docState.CancelInformation.CancelCommandID)
		docState.DocumentInformation.DocumentStatus = contracts.ResultStatusSuccess
	}

	//persist the final status of cancel-message in current folder
	docmanager.PersistData(log,
		docState.DocumentInformation.DocumentID,
		docState.DocumentInformation.InstanceID,
		appconfig.DefaultLocationOfCurrent, docState)

	//persist : commands execution in completed folder (terminal state folder)
	log.Debugf("Execution of %v is over. Moving interimState file from Current to Completed folder", docState.DocumentInformation.MessageID)

	docmanager.MoveDocumentState(log,
		docState.DocumentInformation.DocumentID,
		docState.DocumentInformation.InstanceID,
		appconfig.DefaultLocationOfCurrent,
		appconfig.DefaultLocationOfCompleted)

}

//TODO remove this once CloudWatch plugin is reworked
//temporary solution on plugins with shared responsibility with agent
func handleCloudwatchPlugin(context context.T, pluginResults map[string]*contracts.PluginResult, documentID string) {
	log := context.Log()
	instanceID, _ := platform.InstanceID()
	//TODO once associaiton service switch to use RC and CW goes away, remove this block
	for ID, pluginRes := range pluginResults {
		if pluginRes.PluginName == appconfig.PluginNameCloudWatch {
			log.Infof("Found %v to invoke lrpm invoker", pluginRes.PluginName)
			orchestrationRootDir := filepath.Join(
				appconfig.DefaultDataStorePath,
				instanceID,
				appconfig.DefaultDocumentRootDirName,
				context.AppConfig().Agent.OrchestrationRootDir)
			orchestrationDir := fileutil.BuildPath(orchestrationRootDir, documentID, pluginRes.PluginName)
			manager.Invoke(log, ID, pluginRes, orchestrationDir)
		}
	}

}
