package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/apache/pulsar-client-go/pulsar"

	"github.com/sentinelgo/synergy-common/pkg/log"
)

// Config is loaded independently of synergy-common's shared config.Config
// (which has no Pulsar fields) via config.FromViper[Config]() against the
// top-level `pulsar:` key in config.yml.
type Config struct {
	Pulsar Settings `json:"pulsar" yaml:"pulsar" mapstructure:"pulsar"`
}

type Settings struct {
	Enabled                  bool   `json:"enabled" yaml:"enabled" mapstructure:"enabled"`
	ServiceURL               string `json:"service_url" yaml:"service-url" mapstructure:"service-url"`
	Topic                    string `json:"topic" yaml:"topic" mapstructure:"topic"`
	ConnectionTimeoutSeconds int    `json:"connection_timeout_seconds" yaml:"connection-timeout-seconds" mapstructure:"connection-timeout-seconds"`
}

func (s Settings) topic() string {
	if s.Topic != "" {
		return s.Topic
	}
	return Topic
}

// PulsarPublisher publishes geofence events to Pulsar via SendAsync — sends
// never block on broker round-trips, and a failed send is only logged (see
// Publish), matching the best-effort/eventually-consistent design in the
// package doc comment.
type PulsarPublisher struct {
	client   pulsar.Client
	producer pulsar.Producer
}

// NewPulsarPublisher dials the configured Pulsar broker and creates a
// producer on settings.Topic (or Topic if unset).
func NewPulsarPublisher(settings Settings) (*PulsarPublisher, error) {
	timeout := time.Duration(settings.ConnectionTimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 10 * time.Second
	}

	client, err := pulsar.NewClient(pulsar.ClientOptions{
		URL:               settings.ServiceURL,
		ConnectionTimeout: timeout,
	})
	if err != nil {
		return nil, fmt.Errorf("pulsar: connect to %q: %w", settings.ServiceURL, err)
	}

	producer, err := client.CreateProducer(pulsar.ProducerOptions{
		Topic: settings.topic(),
	})
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("pulsar: create producer on %q: %w", settings.topic(), err)
	}

	return &PulsarPublisher{client: client, producer: producer}, nil
}

// Publish marshals event and sends it asynchronously. It never returns an
// error to the caller — a failed marshal or broker send is logged via
// log.LoggerFromContext(ctx) and otherwise swallowed, since event delivery
// is best-effort and must never fail the HTTP request that triggered it.
func (p *PulsarPublisher) Publish(ctx context.Context, event Event) {
	l := log.LoggerFromContext(ctx)

	body, err := json.Marshal(event)
	if err != nil {
		l.With("error", err, "event_type", event.EventType, "geofence_id", event.GeofenceID).Error("failed to marshal geofence event")
		return
	}

	p.producer.SendAsync(ctx, &pulsar.ProducerMessage{
		Key:     event.GeofenceID.String(),
		Payload: body,
	}, func(_ pulsar.MessageID, _ *pulsar.ProducerMessage, err error) {
		if err != nil {
			l.With("error", err, "event_type", event.EventType, "geofence_id", event.GeofenceID).Error("failed to publish geofence event")
		}
	})
}

func (p *PulsarPublisher) Close() {
	p.producer.Close()
	p.client.Close()
}
