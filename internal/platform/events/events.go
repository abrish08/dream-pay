// Package events implements reliable event publishing with a transactional
// outbox, per module.
//
// A module publishes by writing to its OWN outbox table inside the same
// database transaction as the business change. The module's relay then hands
// outbox rows to the central bus. Consumers wrap their work in HandleOnce so
// at-least-once delivery never applies an event twice.
package events

import (
	"context"

	"gorm.io/gorm"

	"paygate/internal/platform/bus"
)

// Publisher writes an event to the calling module's outbox inside tx.
type Publisher interface {
	Publish(ctx context.Context, tx *gorm.DB, evt bus.Event) error
}
