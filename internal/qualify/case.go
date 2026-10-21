// Package qualify - the domain model of qualification: the case and its statuses, the lead's
// facts, tariffs, niche and the meeting booking gate. Decisions here are deterministic:
// the model only brings facts, this code judges.
package qualify

import (
	"slices"
	"strings"
	"time"
)

// Status - the state of a case.
type Status string

const (
	StatusCollecting       Status = "collecting"
	StatusReadyToDecide    Status = "ready_to_decide"
	StatusMeetingScheduled Status = "meeting_scheduled"
	StatusRejected         Status = "rejected"
	StatusStopped          Status = "stopped"
	StatusManualReview     Status = "manual_review"
	// StatusRefused - the lead refused on their own. The bot does not write first, but the lead's incoming message
	// returns the case to collection with the facts kept.
	StatusRefused Status = "refused"
)

// Frozen - the case is closed: an incoming message is saved but does not wake a turn.
func (s Status) Frozen() bool {
	return s == StatusRejected || s == StatusStopped || s == StatusManualReview
}

// Live - the case is in conversation: an incoming message queues it for a turn.
func (s Status) Live() bool {
	return s == StatusCollecting || s == StatusReadyToDecide || s == StatusMeetingScheduled
}

// Lead - the interlocutor. PeerRef is the address in the transport, CRMRef is the lead card.
type Lead struct {
	ID      int64
	Name    string
	PeerRef string
	CRMRef  string
}

// Case - the case projection. Facts are a trusted snapshot after Go validation.
type Case struct {
	ID             int64
	LeadID         int64
	Status         Status
	Facts          Facts
	DecisionReason string
	Attempts       int
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

const (
	DirectionIn  = "in"
	DirectionOut = "out"
)

// Message - a message in the case history.
type Message struct {
	ID        int64
	CaseID    int64
	Direction string
	Text      string
	CreatedAt time.Time
}

// OfferedSlot - a window shown to the lead. ManagerID is whose set was shown, but not
// a promise: on booking the named hour is taken by a free manager with the lower
// Priority, the lead chooses a time, not a person.
type OfferedSlot struct {
	ManagerID string
	Start     time.Time
	End       time.Time
}

// Meeting - the meeting booking. Status: scheduled or cancelled.
type Meeting struct {
	ID        int64
	CaseID    int64
	ManagerID string
	Start     time.Time
	End       time.Time
	Status    string
}

const (
	MeetingScheduled = "scheduled"
	MeetingCancelled = "cancelled"
)

// DecisionInput - the case snapshot at the moment of a turn. Now is the turn moment, set by
// the caller: tests and eval pass a fixed time.
type DecisionInput struct {
	Case                  Case
	Lead                  Lead
	Messages              []Message
	LastIncomingMessageID int64
	OfferedSlots          []OfferedSlot
	// Meeting - the active booking of the case; nil if there is none. ManagerName is the name
	// of the booking manager from the catalog, filled by dialog.
	Meeting     *Meeting
	ManagerName string
	Now         time.Time
}

// LeadBlock - the lead's messages the turn answers: all incoming ones after the
// last outgoing one.
func (in DecisionInput) LeadBlock() string {
	var block []string
	for index := len(in.Messages) - 1; index >= 0; index-- {
		message := in.Messages[index]
		if message.Direction == DirectionOut {
			break
		}
		block = append(block, message.Text)
	}
	slices.Reverse(block)
	return strings.Join(block, "\n")
}

// CompleteIsIdempotent - a repeated complete does not recompute the gate: the lead
// already got the windows. A fact correction in ready_to_decide overrides this: a changed
// fact must pass the gate again. The gate does not review an appointed meeting:
// it does not cancel the booking, and the lead would be left with a refusal on top of a booking.
func CompleteIsIdempotent(status Status, factsChanged bool) bool {
	return status == StatusMeetingScheduled || (status == StatusReadyToDecide && !factsChanged)
}
