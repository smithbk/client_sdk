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

package peer

import (
	"fmt"
	"time"

	"github.com/golang/protobuf/proto"
	"github.com/looplab/fsm"
	"github.com/spf13/viper"

	pb "github.com/openblockchain/obc-peer/protos"
)

// Handler peer handler implementation.
type Handler struct {
	ToPeerEndpoint  *pb.PeerEndpoint
	Coordinator     MessageHandlerCoordinator
	ChatStream      ChatStream
	doneChan        chan bool
	FSM             *fsm.FSM
	initiatedStream bool // Was the stream initiated within this Peer
}

// NewPeerHandler returns a new Peer handler
func NewPeerHandler(coord MessageHandlerCoordinator, stream ChatStream, initiatedStream bool, nextHandler MessageHandler) (MessageHandler, error) {

	d := &Handler{
		ChatStream:      stream,
		initiatedStream: initiatedStream,
		Coordinator:     coord,
	}
	d.doneChan = make(chan bool)

	d.FSM = fsm.NewFSM(
		"created",
		fsm.Events{
			{Name: pb.OpenchainMessage_DISC_HELLO.String(), Src: []string{"created"}, Dst: "established"},
			{Name: pb.OpenchainMessage_DISC_GET_PEERS.String(), Src: []string{"established"}, Dst: "established"},
			{Name: pb.OpenchainMessage_DISC_PEERS.String(), Src: []string{"established"}, Dst: "established"},
		},
		fsm.Callbacks{
			"enter_state":                                           func(e *fsm.Event) { d.enterState(e) },
			"before_" + pb.OpenchainMessage_DISC_HELLO.String():     func(e *fsm.Event) { d.beforeHello(e) },
			"before_" + pb.OpenchainMessage_DISC_GET_PEERS.String(): func(e *fsm.Event) { d.beforeGetPeers(e) },
			"before_" + pb.OpenchainMessage_DISC_PEERS.String():     func(e *fsm.Event) { d.beforePeers(e) },
		},
	)

	// If the stream was initiated from this Peer, send an Initial HELLO message
	if d.initiatedStream {
		// Send intiial Hello
		peerEndpoint, err := GetPeerEndpoint()
		if err != nil {
			return nil, fmt.Errorf("Error getting new Peer handler: %s", err)
		}
		data, err := proto.Marshal(peerEndpoint)
		if err != nil {
			return nil, fmt.Errorf("Error marshalling peerEndpoint: %s", err)
		}
		if err := d.ChatStream.Send(&pb.OpenchainMessage{Type: pb.OpenchainMessage_DISC_HELLO, Payload: data}); err != nil {
			return nil, fmt.Errorf("Error creating new Peer Handler, error returned sending %s: %s", pb.OpenchainMessage_DISC_HELLO, err)
		}
	}

	return d, nil
}

func (d *Handler) enterState(e *fsm.Event) {
	peerLogger.Debug("The Peer's bi-directional stream to %s is %s, from event %s\n", d.ToPeerEndpoint, e.Dst, e.Event)
}

// To return the PeerEndpoint this Handler is connected to.
func (d *Handler) To() (pb.PeerEndpoint, error) {
	return *(d.ToPeerEndpoint), nil
}

// Stop stops this handler, which will trigger the Deregister from the MessageHandlerCoordinator.
func (d *Handler) Stop() error {
	// Deregister the handler
	err := d.Coordinator.DeregisterHandler(d)
	d.doneChan <- true
	if err != nil {
		return fmt.Errorf("Error stopping MessageHandler: %s", err)
	}
	return nil
}

func (d *Handler) beforeHello(e *fsm.Event) {
	peerLogger.Debug("Received %s, parsing out Peer identification", e.Event)
	// Parse out the PeerEndpoint information
	if _, ok := e.Args[0].(*pb.OpenchainMessage); !ok {
		e.Cancel(fmt.Errorf("Received unexpected message type"))
		return
	}
	msg := e.Args[0].(*pb.OpenchainMessage)

	peerEndpoint := &pb.PeerEndpoint{}
	err := proto.Unmarshal(msg.Payload, peerEndpoint)
	if err != nil {
		e.Cancel(fmt.Errorf("Error unmarshalling PeerEndpoint: %s", err))
		return
	}
	// Store the PeerEndpoint
	d.ToPeerEndpoint = peerEndpoint
	peerLogger.Debug("Received %s from endpoint=%s", e.Event, peerEndpoint)
	if d.initiatedStream == false {
		// Did NOT intitiate the stream, need to send back HELLO
		peerLogger.Debug("Received %s, sending back %s", e.Event, pb.OpenchainMessage_DISC_HELLO.String())
		// Send back out PeerID information in a Hello
		peerEndpoint, err := GetPeerEndpoint()
		if err != nil {
			e.Cancel(fmt.Errorf("Error in processing %s: %s", e.Event, err))
			return
		}
		data, err := proto.Marshal(peerEndpoint)
		if err != nil {
			e.Cancel(fmt.Errorf("Error marshalling peerEndpoint: %s", err))
			return
		}
		if err := d.ChatStream.Send(&pb.OpenchainMessage{Type: pb.OpenchainMessage_DISC_HELLO, Payload: data}); err != nil {
			e.Cancel(fmt.Errorf("Error sending response to %s:  %s", e.Event, err))
			return
		}
	}
	// Register
	err = d.Coordinator.RegisterHandler(d)
	if err != nil {
		e.Cancel(fmt.Errorf("Error registering Handler: %s", err))
	}
	go d.start()
}

func (d *Handler) beforeGetPeers(e *fsm.Event) {
	peersMessage, err := d.Coordinator.GetPeers()
	if err != nil {
		e.Cancel(fmt.Errorf("Error Getting Peers: %s", err))
		return
	}
	data, err := proto.Marshal(peersMessage)
	if err != nil {
		e.Cancel(fmt.Errorf("Error Marshalling PeersMessage: %s", err))
		return
	}
	peerLogger.Debug("Sending back %s", pb.OpenchainMessage_DISC_PEERS.String())
	if err := d.ChatStream.Send(&pb.OpenchainMessage{Type: pb.OpenchainMessage_DISC_PEERS, Payload: data}); err != nil {
		e.Cancel(err)
	}
}

func (d *Handler) beforePeers(e *fsm.Event) {
	peerLogger.Debug("Received %s, grabbing peers message", e.Event)
	// Parse out the PeerEndpoint information
	if _, ok := e.Args[0].(*pb.OpenchainMessage); !ok {
		e.Cancel(fmt.Errorf("Received unexpected message type"))
		return
	}
	msg := e.Args[0].(*pb.OpenchainMessage)

	peersMessage := &pb.PeersMessage{}
	err := proto.Unmarshal(msg.Payload, peersMessage)
	if err != nil {
		e.Cancel(fmt.Errorf("Error unmarshalling PeersMessage: %s", err))
		return
	}

	peerLogger.Debug("Received PeersMessage with Peers: %s", peersMessage)
	d.Coordinator.PeersDiscovered(peersMessage)

	// // Can be used to demonstrate Broadcast function
	// if viper.GetString("peer.id") == "jdoe" {
	// 	d.Coordinator.Broadcast(&pb.OpenchainMessage{Type: pb.OpenchainMessage_UNDEFINED})
	// }

}

func (d *Handler) when(stateToCheck string) bool {
	return d.FSM.Is(stateToCheck)
}

// HandleMessage handles the Openchain messages for the Peer.
func (d *Handler) HandleMessage(msg *pb.OpenchainMessage) error {
	peerLogger.Debug("Handling OpenchainMessage of type: %s ", msg.Type)
	if d.FSM.Cannot(msg.Type.String()) {
		return fmt.Errorf("Peer FSM cannot handle message (%s) with payload size (%d) while in state: %s", msg.Type.String(), len(msg.Payload), d.FSM.Current())
	}
	err := d.FSM.Event(msg.Type.String(), msg)
	if err != nil {
		if _, ok := err.(*fsm.NoTransitionError); !ok {
			// Only allow NoTransitionError's, all others are considered true error.
			return fmt.Errorf("Peer FSM failed while handling message (%s): current state: %s, error: %s", msg.Type.String(), d.FSM.Current(), err)
			//t.Error("expected only 'NoTransitionError'")
		}
	}
	return nil
}

// SendMessage sends a message to the remote PEER through the stream
func (d *Handler) SendMessage(msg *pb.OpenchainMessage) error {
	peerLogger.Debug("Sending message to stream of type: %s ", msg.Type)
	err := d.ChatStream.Send(msg)
	if err != nil {
		return fmt.Errorf("Error Sending message through ChatStream: %s", err)
	}
	return nil
}

// start starts the Peer server function
func (d *Handler) start() error {
	discPeriod := viper.GetDuration("peer.discovery.period")
	tickChan := time.NewTicker(discPeriod).C
	peerLogger.Debug("Starting Peer discovery service")
	for {
		select {
		case <-tickChan:
			if err := d.ChatStream.Send(&pb.OpenchainMessage{Type: pb.OpenchainMessage_DISC_GET_PEERS}); err != nil {
				peerLogger.Error(fmt.Sprintf("Error sending %s during handler discovery tick: %s", pb.OpenchainMessage_DISC_GET_PEERS, err))
			}
		case <-d.doneChan:
			peerLogger.Debug("Stopping discovery service")
			return nil
		}
	}
}
