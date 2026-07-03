package events

import "context"

// NoopPublisher discards every event. Used when Pulsar is disabled
// (pulsar.enabled: false in config.yml), e.g. for local development without
// a broker.
type NoopPublisher struct{}

func (NoopPublisher) Publish(context.Context, Event) {}

func (NoopPublisher) Close() {}
