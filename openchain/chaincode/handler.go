/*
Licensed to the Apache Software Foundation (ASF) under one
or more contributor license agreements.  See the NOTICE file
distributed with this work for additional information
regarding copyright ownership.  The ASF licenses this file
to you under the Apache License, Version 2.0 (the
"License"); you may not use this file except in compliance
with the License.  You may obtain a copy of the License at

  http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing,
software distributed under the License is distributed on an
"AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
KIND, either express or implied.  See the License for the
specific language governing permissions and limitations
under the License.
*/

package chaincode

import (
	"fmt"
	"io"
	"sync"

	"github.com/golang/protobuf/proto"
	"github.com/looplab/fsm"
	"github.com/op/go-logging"
	pb "github.com/openblockchain/obc-peer/protos"

	"github.com/openblockchain/obc-peer/openchain/ledger"
)

const (
	//FSM states
	CREATED_STATE		= "created"	//start state
	ESTABLISHED_STATE	= "established"	//in: CREATED, rcv:  REGISTER, send: REGISTERED, INIT
	INIT_STATE		= "init"	//in:ESTABLISHED, rcv:-, send: INIT
	READY_STATE		= "ready"	//in:ESTABLISHED,TRANSACTION, rcv:COMPLETED
	TRANSACTION_STATE	= "transaction"	//in:READY, rcv: xact from consensus, send: TRANSACTION
	BUSYINIT_STATE		= "busyinit"	//in:INIT, rcv: PUT_STATE, DEL_STATE, INVOKE_CHAINCODE 
	BUSYXACT_STATE		= "busyxact"	//in:TRANSACION, rcv: PUT_STATE, DEL_STATE, INVOKE_CHAINCODE
	END_STATE		= "end"		//in:INIT,ESTABLISHED, rcv: error, terminate container

)

var chaincodeLogger = logging.MustGetLogger("chaincode")

// PeerChaincodeStream interface for stream between Peer and chaincode instance.
type PeerChaincodeStream interface {
	Send(*pb.ChaincodeMessage) error
	Recv() (*pb.ChaincodeMessage, error)
}

// MessageHandler interface for handling chaincode messages (common between Peer chaincode support and chaincode)
type MessageHandler interface {
	HandleMessage(msg *pb.ChaincodeMessage) error
	SendMessage(msg *pb.ChaincodeMessage) error
}

// Handler responsbile for managment of Peer's side of chaincode stream
type Handler struct {
	sync.RWMutex
	ChatStream      PeerChaincodeStream
	FSM             *fsm.FSM
	ChaincodeID     *pb.ChainletID
	chainletSupport *ChainletSupport
	registered      bool
	readyNotify	chan bool
	responseNotifiers map[string] chan *pb.ChaincodeMessage
	// Uuids of all in-progress state invocations
	uuidMap 	map[string]bool
}

func (handler *Handler) deregister() error {
	if handler.registered {
		handler.chainletSupport.deregisterHandler(handler)
	}
	return nil
}

func (handler *Handler) processStream() error {
	defer handler.deregister()
	for {
		in, err := handler.ChatStream.Recv()
		// Defer the deregistering of the this handler.
		if err == io.EOF {
			chaincodeLogger.Debug("Received EOF, ending chaincode support stream")
			return err
		}
		if err != nil {
			chainletLog.Error(fmt.Sprintf("Error handling chaincode support stream: %s", err))
			return err
		}
		err = handler.HandleMessage(in)
		if err != nil {
			return fmt.Errorf("Error handling message, ending stream: %s", err)
		}
	}
}

// HandleChaincodeStream Main loop for handling the associated Chaincode stream
func HandleChaincodeStream(chainletSupport *ChainletSupport, stream pb.ChainletSupport_RegisterServer) error {
	deadline, ok := stream.Context().Deadline()
	chaincodeLogger.Debug("Current context deadline = %s, ok = %v", deadline, ok)
	handler := newChaincodeSupportHandler(chainletSupport, stream)
	return handler.processStream()
}

func newChaincodeSupportHandler(chainletSupport *ChainletSupport, peerChatStream PeerChaincodeStream) *Handler {
	v := &Handler{
		ChatStream: peerChatStream,
	}
	v.chainletSupport = chainletSupport

	v.FSM = fsm.NewFSM(
		CREATED_STATE,
		fsm.Events{
			//Send REGISTERED, then, if deploy { trigger INIT(via INIT) } else { trigger READY(via COMPLETED) }
			{Name: pb.ChaincodeMessage_REGISTER.String(), Src: []string{CREATED_STATE}, Dst: ESTABLISHED_STATE},
			{Name: pb.ChaincodeMessage_INIT.String(), Src: []string{ESTABLISHED_STATE}, Dst: INIT_STATE},
			{Name: pb.ChaincodeMessage_READY.String(), Src: []string{ESTABLISHED_STATE}, Dst: READY_STATE},
			{Name: pb.ChaincodeMessage_TRANSACTION.String(), Src: []string{READY_STATE}, Dst: TRANSACTION_STATE},
			{Name: pb.ChaincodeMessage_PUT_STATE.String(), Src: []string{TRANSACTION_STATE}, Dst: BUSYXACT_STATE},
			{Name: pb.ChaincodeMessage_DEL_STATE.String(), Src: []string{TRANSACTION_STATE}, Dst: BUSYXACT_STATE},
			{Name: pb.ChaincodeMessage_INVOKE_CHAINCODE.String(), Src: []string{TRANSACTION_STATE}, Dst: BUSYXACT_STATE},
			{Name: pb.ChaincodeMessage_PUT_STATE.String(), Src: []string{INIT_STATE}, Dst: BUSYINIT_STATE},
			{Name: pb.ChaincodeMessage_DEL_STATE.String(), Src: []string{INIT_STATE}, Dst: BUSYINIT_STATE},
			{Name: pb.ChaincodeMessage_INVOKE_CHAINCODE.String(), Src: []string{INIT_STATE}, Dst: BUSYINIT_STATE},
			{Name: pb.ChaincodeMessage_COMPLETED.String(), Src: []string{INIT_STATE,TRANSACTION_STATE}, Dst: READY_STATE}, 
			{Name: pb.ChaincodeMessage_GET_STATE.String(), Src: []string{INIT_STATE}, Dst: INIT_STATE},
			{Name: pb.ChaincodeMessage_GET_STATE.String(), Src: []string{BUSYINIT_STATE}, Dst: BUSYINIT_STATE},
			{Name: pb.ChaincodeMessage_GET_STATE.String(), Src: []string{TRANSACTION_STATE}, Dst: TRANSACTION_STATE},
			{Name: pb.ChaincodeMessage_GET_STATE.String(), Src: []string{BUSYXACT_STATE}, Dst: BUSYXACT_STATE},
			{Name: pb.ChaincodeMessage_ERROR.String(), Src: []string{INIT_STATE}, Dst: END_STATE},
			{Name: pb.ChaincodeMessage_ERROR.String(), Src: []string{TRANSACTION_STATE}, Dst: READY_STATE},
			{Name: pb.ChaincodeMessage_ERROR.String(), Src: []string{BUSYINIT_STATE}, Dst: INIT_STATE},
			{Name: pb.ChaincodeMessage_ERROR.String(), Src: []string{BUSYXACT_STATE}, Dst: TRANSACTION_STATE},
			{Name: pb.ChaincodeMessage_RESPONSE.String(), Src: []string{BUSYINIT_STATE}, Dst: INIT_STATE},
			{Name: pb.ChaincodeMessage_RESPONSE.String(), Src: []string{BUSYXACT_STATE}, Dst: TRANSACTION_STATE},
		},
		fsm.Callbacks{
			"before_" + pb.ChaincodeMessage_REGISTER.String(): func(e *fsm.Event) { v.beforeRegisterEvent(e, v.FSM.Current()) },
			"before_" + pb.ChaincodeMessage_COMPLETED.String(): func(e *fsm.Event) { v.beforeCompletedEvent(e, v.FSM.Current()) },
			"before_" + pb.ChaincodeMessage_INIT.String(): func(e *fsm.Event) { v.beforeInitState(e, v.FSM.Current()) },
			"before_" + pb.ChaincodeMessage_GET_STATE.String(): func(e *fsm.Event) { v.beforeGetState(e, v.FSM.Current()) },
			"before_" + pb.ChaincodeMessage_PUT_STATE.String(): func(e *fsm.Event) { v.beforePutState(e, v.FSM.Current()) },
			"before_" + pb.ChaincodeMessage_DEL_STATE.String(): func(e *fsm.Event) { v.beforeDelState(e, v.FSM.Current()) },
			"enter_" + ESTABLISHED_STATE: func(e *fsm.Event) { v.enterEstablishedState(e, v.FSM.Current()) },
			"enter_" + READY_STATE: func(e *fsm.Event) { v.enterReadyState(e, v.FSM.Current()) },
			"enter_" + BUSYINIT_STATE: func(e *fsm.Event) { v.enterBusyInitState(e, v.FSM.Current()) },
			"enter_" + BUSYXACT_STATE: func(e *fsm.Event) { v.enterBusyXactState(e, v.FSM.Current()) },
			"enter_" + TRANSACTION_STATE: func(e *fsm.Event) { v.enterTransactionState(e, v.FSM.Current()) },
			"enter_" + END_STATE: func(e *fsm.Event) { v.enterEndState(e, v.FSM.Current()) },
		},
	)
	return v
}

func (handler *Handler) createUuidEntry(uuid string) bool {
	if handler.uuidMap == nil {
		return false
	}
	handler.Lock()
	defer handler.Unlock()
	if handler.uuidMap[uuid] {
		return false
	}
	handler.uuidMap[uuid] = true
	return handler.uuidMap[uuid]
}

func (handler *Handler) deleteUuidEntry(uuid string) {
	handler.Lock()
	if handler.uuidMap != nil {
		delete(handler.uuidMap,uuid)
	}
	handler.Unlock()
}

func (handler *Handler) notifyDuringStartup(val bool) {
	//if USER_RUNS_CC readyNotify will be nil
	if handler.readyNotify != nil {
		handler.readyNotify <- val
	}
}

// beforeRegisterEvent is invoked when chaincode tries to register.
func (handler *Handler) beforeRegisterEvent(e *fsm.Event, state string) {
	chaincodeLogger.Debug("Received %s in state %s", e.Event, state)
	msg, ok := e.Args[0].(*pb.ChaincodeMessage)
	if !ok {
		e.Cancel(fmt.Errorf("Received unexpected message type"))
		return
	}
	chainletID := &pb.ChainletID{}
	err := proto.Unmarshal(msg.Payload, chainletID)
	if err != nil {
		e.Cancel(fmt.Errorf("Error in received %s, could NOT unmarshal registration info: %s", pb.ChaincodeMessage_REGISTER, err))
		return
	}

	// Now register with the chainletSupport
	handler.ChaincodeID = chainletID
	err = handler.chainletSupport.registerHandler(handler)
	if err != nil {
		handler.notifyDuringStartup(false)
		e.Cancel(err)
		return
	}

	chaincodeLogger.Debug("Got %s for chainldetID = %s, sending back %s", e.Event, chainletID, pb.ChaincodeMessage_REGISTERED)
	if err := handler.ChatStream.Send(&pb.ChaincodeMessage{Type: pb.ChaincodeMessage_REGISTERED}); err != nil {
		handler.notifyDuringStartup(false)
		e.Cancel(fmt.Errorf("Error sending %s: %s", pb.ChaincodeMessage_REGISTERED, err))
		return
	}
	handler.notifyDuringStartup(true)
}

func (handler *Handler) notify(msg *pb.ChaincodeMessage) {
	handler.Lock()
	defer handler.Unlock()
	notfy := handler.responseNotifiers[msg.Uuid]
	if notfy == nil {
		fmt.Printf("notifier Uuid:%s does not exist\n", msg.Uuid)
	} else {
		notfy<-msg
		fmt.Printf("notified Uuid:%s\n", msg.Uuid)
	}
}

// beforeCompletedEvent is invoked when chaincode has completed execution of init, invoke or query.
func (handler *Handler) beforeCompletedEvent(e *fsm.Event, state string) {
	chaincodeLogger.Debug("Received %s in state %s", e.Event, state)
	msg, ok := e.Args[0].(*pb.ChaincodeMessage)
	if !ok {
		e.Cancel(fmt.Errorf("Received unexpected message type"))
		return
	}
	chaincodeLogger.Debug("beforeCompleted uuid:%s", msg.Uuid)
	// Now notify
	handler.notify(msg)

	return
}

// beforeInitState is invoked before an init message is sent to the chaincode.
func (handler *Handler) beforeInitState(e *fsm.Event, state string) {
	chaincodeLogger.Debug("Before state %s.. notifying waiter that we are up", state)
	handler.notifyDuringStartup(true)
}

// beforeGetState handles a GET_STATE request from the chaincode.
func (handler *Handler) beforeGetState(e *fsm.Event, state string) {
	msg, ok := e.Args[0].(*pb.ChaincodeMessage)
	if !ok {
		e.Cancel(fmt.Errorf("Received unexpected message type"))
		return
	}
	chaincodeLogger.Debug("Received %s, invoking get state from ledger", pb.ChaincodeMessage_GET_STATE)

	// Query ledger for state
	go func() {
		// Check if this is the unique state request from this chaincode uuid
		uniqueReq := handler.createUuidEntry(msg.Uuid)
		if !uniqueReq {
			// Drop this request
			chaincodeLogger.Debug("Another state request pending for this Uuid. Cannot process.")
			return
		}

		key := string(msg.Payload)
		ledgerObj, ledgerErr := ledger.GetLedger()
		if ledgerErr != nil {
			// Send error msg back to chaincode. GetState will not trigger event
			payload := []byte(ledgerErr.Error())
			chaincodeLogger.Debug("Failed to get chaincode state. Sending %s", pb.ChaincodeMessage_ERROR)
			errMsg := &pb.ChaincodeMessage{Type: pb.ChaincodeMessage_ERROR, Payload: payload, Uuid: msg.Uuid} 
			handler.ChatStream.Send(errMsg)
			// Remove uuid from current set
			handler.deleteUuidEntry(msg.Uuid)
			return
		}

		// Invoke ledger to get state
		chaincodeID,_ := getChaincodeID(handler.ChaincodeID)
		res, err := ledgerObj.GetState(chaincodeID, key)
		if err != nil {
			// Send error msg back to chaincode. GetState will not trigger event
			payload := []byte(err.Error())
			chaincodeLogger.Debug("Failed to get chaincode state. Sending %s", pb.ChaincodeMessage_ERROR)
			errMsg := &pb.ChaincodeMessage{Type: pb.ChaincodeMessage_ERROR, Payload: payload, Uuid: msg.Uuid} 
			handler.ChatStream.Send(errMsg)
		} else {
			// Send response msg back to chaincode. GetState will not trigger event
			chaincodeLogger.Debug("Got state. Sending %s", pb.ChaincodeMessage_RESPONSE)
			responseMsg := &pb.ChaincodeMessage{Type: pb.ChaincodeMessage_RESPONSE, Payload: res, Uuid: msg.Uuid} 
			handler.ChatStream.Send(responseMsg)
		}

		// Remove uuid from current set
		handler.deleteUuidEntry(msg.Uuid)
	}()
}

// beforePutState handles a PUT_STATE request from the chaincode.
func (handler *Handler) beforePutState(e *fsm.Event, state string) {
	msg, ok := e.Args[0].(*pb.ChaincodeMessage)
	if !ok {
		e.Cancel(fmt.Errorf("Received unexpected message type"))
		return
	}
	chaincodeLogger.Debug("Received %s, invoking get state from ledger", pb.ChaincodeMessage_PUT_STATE)

	// Put state into ledger
	go func() {
		// Check if this is the unique state request from this chaincode uuid
		uniqueReq := handler.createUuidEntry(msg.Uuid)
		if !uniqueReq {
			// Drop this request
			chaincodeLogger.Debug("Another state request pending for this Uuid. Cannot process.")
			return
		}

		putStateInfo := &pb.PutStateInfo{}
		unmarshalErr := proto.Unmarshal(msg.Payload, putStateInfo) 
		if unmarshalErr != nil {
			payload := []byte(unmarshalErr.Error())
			chaincodeLogger.Debug("Unable to decipher payload. Sending %s", pb.ChaincodeMessage_ERROR)
			errMsg := &pb.ChaincodeMessage{Type: pb.ChaincodeMessage_ERROR, Payload: payload, Uuid: msg.Uuid} 
			handler.ChatStream.Send(errMsg)
			// Send FSM event to trigger state change
			handler.FSM.Event(pb.ChaincodeMessage_ERROR.String(), errMsg)
			// Remove uuid from current set
			handler.deleteUuidEntry(msg.Uuid)
			return
		}

		ledgerObj, ledgerErr := ledger.GetLedger()
		if ledgerErr != nil {
			// Send error msg back to chaincode and trigger event
			payload := []byte(ledgerErr.Error())
			chaincodeLogger.Debug("Failed to set chaincode state. Sending %s", pb.ChaincodeMessage_ERROR)
			errMsg := &pb.ChaincodeMessage{Type: pb.ChaincodeMessage_ERROR, Payload: payload, Uuid: msg.Uuid} 
			handler.ChatStream.Send(errMsg)
			// Send FSM event to trigger state change
			handler.FSM.Event(pb.ChaincodeMessage_ERROR.String(), errMsg)
			// Remove uuid from current set
			handler.deleteUuidEntry(msg.Uuid)
			return
		}

		// Invoke ledger to set state
		chaincodeID,_ := getChaincodeID(handler.ChaincodeID)
		err := ledgerObj.SetState(chaincodeID, putStateInfo.Key, putStateInfo.Value)
		if err != nil {
			// Send error msg back to chaincode and trigger event
			payload := []byte(err.Error())
			chaincodeLogger.Debug("Failed to set chaincode state. Sending %s", pb.ChaincodeMessage_ERROR)
			errMsg := &pb.ChaincodeMessage{Type: pb.ChaincodeMessage_ERROR, Payload: payload, Uuid: msg.Uuid} 
			handler.ChatStream.Send(errMsg)
			// Send FSM event to trigger state change
			handler.FSM.Event(pb.ChaincodeMessage_ERROR.String(), errMsg)
		} else {
			// Send response msg back to chaincode. GetState will not trigger event
			chaincodeLogger.Debug("Got state. Sending %s", pb.ChaincodeMessage_RESPONSE)
			var res []byte
			responseMsg := &pb.ChaincodeMessage{Type: pb.ChaincodeMessage_RESPONSE, Payload: res, Uuid: msg.Uuid} 
			handler.ChatStream.Send(responseMsg)
			// Send FSM event to trigger state change
			handler.FSM.Event(pb.ChaincodeMessage_RESPONSE.String(), responseMsg)
		}
		// Remove uuid from current set
		handler.deleteUuidEntry(msg.Uuid)
	}()
}

// beforeDelState handles a DEL_STATE request from the chaincode.
func (handler *Handler) beforeDelState(e *fsm.Event, state string) {
	msg, ok := e.Args[0].(*pb.ChaincodeMessage)
	if !ok {
		e.Cancel(fmt.Errorf("Received unexpected message type"))
		return
	}
	chaincodeLogger.Debug("Received %s, invoking get state from ledger", pb.ChaincodeMessage_DEL_STATE)

	// Delete state from ledger
	go func() {
		// Check if this is the unique state request from this chaincode uuid
		uniqueReq := handler.createUuidEntry(msg.Uuid)
		if !uniqueReq {
			// Drop this request
			chaincodeLogger.Debug("Another state request pending for this Uuid. Cannot process.")
			return
		}

		key := string(msg.Payload)
		ledgerObj, ledgerErr := ledger.GetLedger()
		if ledgerErr != nil {
			// Send error msg back to chaincode and trigger event
			payload := []byte(ledgerErr.Error())
			chaincodeLogger.Debug("Failed to delete chaincode state. Sending %s", pb.ChaincodeMessage_ERROR)
			errMsg := &pb.ChaincodeMessage{Type: pb.ChaincodeMessage_ERROR, Payload: payload, Uuid: msg.Uuid} 
			handler.ChatStream.Send(errMsg)
			// Send FSM event to trigger state change
			handler.FSM.Event(pb.ChaincodeMessage_ERROR.String(), errMsg)
			// Remove uuid from current set
			handler.deleteUuidEntry(msg.Uuid)
			return
		}

		chaincodeID,_ := getChaincodeID(handler.ChaincodeID)
		err := ledgerObj.DeleteState(chaincodeID, key)
		if err != nil {
			// Send error msg back to chaincode and trigger event
			payload := []byte(err.Error())
			chaincodeLogger.Debug("Failed to delete chaincode state. Sending %s", pb.ChaincodeMessage_ERROR)
			errMsg := &pb.ChaincodeMessage{Type: pb.ChaincodeMessage_ERROR, Payload: payload, Uuid: msg.Uuid} 
			handler.ChatStream.Send(errMsg)
			// Send FSM event to trigger state change
			handler.FSM.Event(pb.ChaincodeMessage_ERROR.String(), errMsg)
		} else {
			// Send response msg back to chaincode. DelState will not trigger event
			var res []byte
			chaincodeLogger.Debug("Deleted state. Sending %s", pb.ChaincodeMessage_RESPONSE)
			responseMsg := &pb.ChaincodeMessage{Type: pb.ChaincodeMessage_RESPONSE, Payload: res, Uuid: msg.Uuid} 
			handler.ChatStream.Send(responseMsg)
			// Send FSM event to trigger state change
			handler.FSM.Event(pb.ChaincodeMessage_RESPONSE.String(), responseMsg)
		}

		// Remove uuid from current set
		handler.deleteUuidEntry(msg.Uuid)
	}()
}

func (handler *Handler) enterEstablishedState(e *fsm.Event, state string) {
	chaincodeLogger.Debug("(enterEstablishedState)Entered state %s", state)
}

func (handler *Handler) enterReadyState(e *fsm.Event, state string) {
	chaincodeLogger.Debug("(enterReadyState)Entered state %s", state)
}

func (handler *Handler) enterBusyInitState(e *fsm.Event, state string) {
	chaincodeLogger.Debug("(enterBusyInitState)Entered state %s", state)
}

func (handler *Handler) enterBusyXactState(e *fsm.Event, state string) {
	chaincodeLogger.Debug("(enterBusyXactState)Entered state %s", state)
}

func (handler *Handler) enterTransactionState(e *fsm.Event, state string) {
	chaincodeLogger.Debug("(enterTransactionState)Entered state %s", state)
}

func (handler *Handler) enterEndState(e *fsm.Event, state string) {
	chaincodeLogger.Debug("(enterEndState)Entered state %s", state)
}

//if initArgs is set (should be for "deploy" only) move to Init
//else move to ready
func (handler *Handler) initOrReady(uuid string, f *string, initArgs []string) (chan *pb.ChaincodeMessage, error) {
	var event string
	var notfy chan *pb.ChaincodeMessage
	if f != nil || initArgs != nil {
		chaincodeLogger.Debug("sending INIT")
		var f2 string
		if f != nil {
			f2 = *f
		}
		funcArgsMsg := &pb.ChainletMessage{Function: f2, Args: initArgs}
		payload, err := proto.Marshal(funcArgsMsg)
		if err != nil {
			return nil,err
		}
		notfy,err = handler.createNotifier(uuid)
		if err != nil {
			return nil,err
		}
		ccMsg := &pb.ChaincodeMessage{Type: pb.ChaincodeMessage_INIT, Payload: payload, Uuid: uuid}
		if err = handler.ChatStream.Send(ccMsg); err != nil {
			notfy <- &pb.ChaincodeMessage{Type: pb.ChaincodeMessage_ERROR, Payload: []byte(fmt.Sprintf("Error sending %s: %s", pb.ChaincodeMessage_INIT, err)), Uuid: uuid }
			return notfy, fmt.Errorf("Error sending %s: %s", pb.ChaincodeMessage_INIT, err)
		}
		event = pb.ChaincodeMessage_INIT.String()
	} else {
		chaincodeLogger.Debug("sending READY")
		event = pb.ChaincodeMessage_READY.String()
		//TODO this is really cheating... I should really notify when the state moves to READY...
		//but this is an internal move(not from chaincode, so lets just do it for now)
		notfy <- &pb.ChaincodeMessage{Type: pb.ChaincodeMessage_ERROR, Uuid: uuid }
	}
	err := handler.FSM.Event(event)
	if err != nil {
		fmt.Printf("Err : %s\n", err)
	} else {
		fmt.Printf("Successful event initiation\n")
	}
	return notfy, err
}

// HandleMessage implementation of MessageHandler interface.  Peer's handling of Chaincode messages.
func (handler *Handler) HandleMessage(msg *pb.ChaincodeMessage) error {
	chaincodeLogger.Debug("Handling ChaincodeMessage of type: %s in state %s", msg.Type, handler.FSM.Current())
	if handler.FSM.Cannot(msg.Type.String()) {
		return fmt.Errorf("Chaincode handler FSM cannot handle message (%s) with payload size (%d) while in state: %s", msg.Type.String(), len(msg.Payload), handler.FSM.Current())
	}
	err := handler.FSM.Event(msg.Type.String(), msg)

	return filterError(err)
}

// Filter the Errors to allow NoTransitionError and CanceledError to not propogate for cases where embedded Err == nil
func filterError(errFromFSMEvent error) error {
	if errFromFSMEvent != nil {
		if noTransitionErr, ok := errFromFSMEvent.(*fsm.NoTransitionError); ok {
			if noTransitionErr.Err != nil {
				// Only allow NoTransitionError's, all others are considered true error.
				return errFromFSMEvent
			}
			chaincodeLogger.Debug("Ignoring NoTransitionError: %s", noTransitionErr)
		}
		if canceledErr, ok := errFromFSMEvent.(*fsm.CanceledError); ok {
			if canceledErr.Err != nil {
				// Only allow NoTransitionError's, all others are considered true error.
				return canceledErr
				//t.Error("expected only 'NoTransitionError'")
			}
			chaincodeLogger.Debug("Ignoring CanceledError: %s", canceledErr)
		}
	}
	return nil
}

func (handler *Handler) deleteNotifier(uuid string) {
	handler.Lock()
	if handler.responseNotifiers != nil {
		delete(handler.responseNotifiers,uuid)
	}
	handler.Unlock()
}
func (handler *Handler) createNotifier(uuid string) (chan *pb.ChaincodeMessage, error) {
	if handler.responseNotifiers == nil {
		return nil,fmt.Errorf("cannot create notifier for Uuid:%s", uuid)
	}
	handler.Lock()
	defer handler.Unlock()
	if handler.responseNotifiers[uuid] != nil {
		return nil, fmt.Errorf("Uuid:%s exists", uuid)
	}
	handler.responseNotifiers[uuid] = make(chan *pb.ChaincodeMessage, 1)
	return handler.responseNotifiers[uuid],nil
}

func (handler *Handler) sendExecuteMessage(msg *pb.ChaincodeMessage) (chan *pb.ChaincodeMessage, error) {
	notfy,err := handler.createNotifier(msg.Uuid)
	if err != nil {
		return nil, err
	}
	if err := handler.ChatStream.Send(msg); err != nil {
		handler.deleteNotifier(msg.Uuid)
		return nil, fmt.Errorf("SendMessage error sending %s(%s)", msg.Uuid, err)
	}

	if msg.Type.String() == pb.ChaincodeMessage_TRANSACTION.String() {
		handler.FSM.Event(msg.Type.String(), msg)
	}
	return notfy, nil
}

/****************
func (handler *Handler) initEvent() (chan *pb.ChaincodeMessage, error) {
	if handler.responseNotifiers == nil {
		return nil,fmt.Errorf("SendMessage called before registration for Uuid:%s", msg.Uuid)
	}
	var notfy chan *pb.ChaincodeMessage
	handler.Lock()
	if handler.responseNotifiers[msg.Uuid] != nil {
		handler.Unlock()
		return nil, fmt.Errorf("SendMessage Uuid:%s exists", msg.Uuid)
	}
	//note the explicit use of buffer 1. We won't block if the receiver times outi and does not wait
	//for our response
	handler.responseNotifiers[msg.Uuid] = make(chan *pb.ChaincodeMessage, 1)
	handler.Unlock()

	if err := c.ChatStream.Send(msg); err != nil {
		deleteNotifier(msg.Uuid)
		return nil, fmt.Errorf("SendMessage error sending %s(%s)", msg.Uuid, err)
	}
	return notfy, nil
}
*******************/
