/*
Copyright 2020 Google LLC
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at
    http://www.apache.org/licenses/LICENSE-2.0
Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"log"
	"time"

	"knative.dev/pkg/signals"

	cloudevents "github.com/cloudevents/sdk-go/v2"
	"github.com/cloudevents/sdk-go/v2/protocol"
	"github.com/kelseyhightower/envconfig"
)

type cloudEventsFunc func(cloudevents.Event) protocol.Result

type envConfig struct {
	// Environment variable containing the sink URL (broker URL) that the event will be forwarded to by the probeHelper
	BrokerURL string `envconfig:"K_SINK" default:"http://default-brokercell-ingress.cloud-run-events.svc.cluster.local/cloud-run-events-probe/default"`

	// Environment variable containing the port which listens to the probe to deliver the event
	ProbePort int `envconfig:"PROBE_PORT" default:"8070"`

	// Environment variable containing the port to receive the event from the trigger
	ReceiverPort int `envconfig:"RECEIVER_PORT" default:"8080"`

	// Environment variable containing the timeout period to wait for an event to be delivered back (in minutes)
	Timeout int `envconfig:"TIMEOUT_MINS" default:"30"`
}

func forwardFromProbe(ctx context.Context, c cloudevents.Client, receivedEvents map[string]chan bool, timeout int) cloudEventsFunc {
	return func(event cloudevents.Event) protocol.Result {
		log.Printf("Received probe request: %+v \n", event)
		eventID := event.ID()
		receivedEvents[eventID] = make(chan bool, 1)
		ctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Minute)
		defer cancel()
		if res := c.Send(ctx, event); !cloudevents.IsACK(res) {
			return res
		}
		select {
		case <-receivedEvents[eventID]:
			delete(receivedEvents, eventID)
			return cloudevents.ResultACK
		case <-ctx.Done():
			return cloudevents.NewReceipt(false, "timed out waiting for event to be sent back")
		}
	}
}

func receiveFromTrigger(receivedEvents map[string]chan bool) cloudEventsFunc {
	return func(event cloudevents.Event) protocol.Result {
		log.Printf("Received event: %+v \n", event)
		eventID := event.ID()
		ch, ok := receivedEvents[eventID]
		if !ok {
			log.Printf("This event is not received by the probe receiver client: %v \n", eventID)
			return cloudevents.ResultACK
		}
		ch <- true
		return cloudevents.ResultACK
	}
}

func runProbeHelper() {
	var env envConfig
	if err := envconfig.Process("", &env); err != nil {
		log.Fatalf("Failed to process env var, %v", err)
	}
	brokerURL := env.BrokerURL
	probePort := env.ProbePort
	receiverPort := env.ReceiverPort
	timeout := env.Timeout
	log.Printf("Running Probe Helper with env config: %+v \n", env)
	// create sender client
	sp, err := cloudevents.NewHTTP(cloudevents.WithPort(probePort), cloudevents.WithTarget(brokerURL))
	if err != nil {
		log.Fatalf("Failed to create sender transport, %v", err)
	}
	sc, err := cloudevents.NewClient(sp)
	if err != nil {
		log.Fatal("Failed to create sender client, ", err)
	}
	// create receiver client
	rp, err := cloudevents.NewHTTP(cloudevents.WithPort(receiverPort))
	if err != nil {
		log.Fatalf("Failed to create receiver transport, %v", err)
	}
	rc, err := cloudevents.NewClient(rp)
	if err != nil {
		log.Fatal("Failed to create receiver client, ", err)
	}
	// make a map to store the channel for each event
	receivedEvents := make(map[string]chan bool)
	ctx := signals.NewContext()
	// start a goroutine to receive the event from probe and forward the event to the broker
	log.Println("Starting Probe Helper server...")
	go sc.StartReceiver(ctx, forwardFromProbe(ctx, sc, receivedEvents, timeout))
	// Receive the event from the trigger and return the result back to the probe
	log.Println("Starting event receiver...")
	rc.StartReceiver(ctx, receiveFromTrigger(receivedEvents))
}
