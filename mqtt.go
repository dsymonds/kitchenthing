package main

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"strconv"

	"github.com/eclipse/paho.golang/autopaho"
	"github.com/eclipse/paho.golang/paho"
)

const (
	mqttClientID = "kitchenthing"
)

type MQTT struct {
	cm *autopaho.ConnectionManager
}

func NewMQTT(cfg Config) (*MQTT, error) {
	if cfg.MQTT == "" {
		return nil, nil
	}

	broker, err := url.Parse(cfg.MQTT)
	if err != nil {
		return nil, fmt.Errorf("parsing MQTT broker addr %q: %v", cfg.MQTT, err)
	}

	mqtt := &MQTT{}

	// Ensure OnConnectionUp won't race us.
	initc := make(chan int)
	defer close(initc)

	log.Printf("MQTT connecting to broker at %v", broker)
	cm, err := autopaho.NewConnection(context.Background(), autopaho.ClientConfig{
		BrokerUrls: []*url.URL{broker},
		KeepAlive:  10, // seconds
		OnConnectionUp: func(cm *autopaho.ConnectionManager, connAck *paho.Connack) {
			log.Printf("MQTT connection up")
			<-initc          // wait until NewMQTT returns
			mqtt.discovery() // TODO: only once?
		},
		OnConnectError: func(err error) {
			//log.Printf("Connection error: %v", err)
		},
		//PahoErrors: pahoLogger{log, "ERROR"},

		// TODO: wire up Debug/PahoDebug for more details?

		ClientConfig: paho.ClientConfig{
			ClientID: mqttClientID,
			// TODO: need OnClientError/OnServerDisconnect?
		},
	})
	if err != nil {
		return nil, fmt.Errorf("preparing MQTT client connection: %w", err)
	}
	mqtt.cm = cm
	return mqtt, nil
}

func (m *MQTT) discovery() {
	// https://www.home-assistant.io/integrations/mqtt/#mqtt-discovery

	ctx := context.Background()
	_, err := m.cm.Publish(ctx, &paho.Publish{
		QoS:     0, // at most once
		Retain:  true,
		Topic:   "homeassistant/sensor/todoist/power_hungry_pending_count/config",
		Payload: []byte(mqttDiscoveryPayload),
	})
	if err != nil {
		log.Printf("Publishing discovery message: %v", err)
	}
}

// Constructed manually, and with a lot of trial and error.
// The HA docs are not clear.
const mqttDiscoveryPayload = `
{
  "name": "power-hungry pending count",
  "object_id": "power_hungry_pending_count",
  "unique_id": "todoist_phpc",
  "state_class": "measurement",
  "retain": true,
  "state_topic": "` + mqttUpdateTopic + `",
  "unit_of_measurement": "tasks",
  "icon": "mdi:checkbox-marked-circle-auto-outline",
  "device": {
    "name": "Todoist meta-device",
    "manufacturer": "Dave Industries",
    "model": "kitchenthing",
    "suggested_area": "Kitchen",
    "identifiers": ["todoist"]
  }
}
`

const mqttUpdateTopic = "todoist/power_hungry_pending_count/value"

func (m *MQTT) PublishUpdate(tasks []renderableTask) error {
	ctx := context.Background()

	// Count number of tasks that have the "power-hungry" label,
	// and do *not* have the "in-progress" label.
	phpc := 0
	for _, t := range tasks {
		if t.PowerHungry && !t.InProgress {
			phpc++
		}
	}

	//log.Printf("Publishing %d to MQTT %s", phpc, mqttUpdateTopic)
	_, err := m.cm.Publish(ctx, &paho.Publish{
		QoS:     0, // at most once
		Retain:  true,
		Topic:   mqttUpdateTopic,
		Payload: []byte(strconv.Itoa(phpc)),
	})
	return err
}
