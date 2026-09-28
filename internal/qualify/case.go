// Package qualify - доменная модель квалификации: кейс и его статусы, факты
// лида, тарифы, ниша и гейт записи на встречу. Решения здесь детерминированные:
// модель только приносит факты, судит этот код.
package qualify

import (
	"slices"
	"strings"
	"time"
)

// Status - состояние кейса.
type Status string

const (
	StatusCollecting       Status = "collecting"
	StatusReadyToDecide    Status = "ready_to_decide"
	StatusMeetingScheduled Status = "meeting_scheduled"
	StatusRejected         Status = "rejected"
	StatusStopped          Status = "stopped"
	StatusManualReview     Status = "manual_review"
	// StatusRefused - лид отказался сам. Бот первым не пишет, но входящее лида
	// возвращает кейс в сбор с сохранёнными фактами.
	StatusRefused Status = "refused"
)

// Frozen - кейс закрыт: входящее сохраняется, но хода не будит.
func (s Status) Frozen() bool {
	return s == StatusRejected || s == StatusStopped || s == StatusManualReview
}

// Live - кейс в разговоре: входящее ставит его в очередь хода.
func (s Status) Live() bool {
	return s == StatusCollecting || s == StatusReadyToDecide || s == StatusMeetingScheduled
}

// Lead - собеседник. PeerRef - адрес в транспорте, CRMRef - карточка заявки.
type Lead struct {
	ID      int64
	Name    string
	PeerRef string
	CRMRef  string
}

// Case - проекция кейса. Факты - доверенный снимок после проверки Go.
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

// Message - реплика в истории кейса.
type Message struct {
	ID        int64
	CaseID    int64
	Direction string
	Text      string
	CreatedAt time.Time
}

// OfferedSlot - окно, показанное лиду. ManagerID - чей набор показан, но не
// обещание: при брони названный час берёт свободный менеджер с меньшим
// Priority, лид выбирает время, а не человека.
type OfferedSlot struct {
	ManagerID string
	Start     time.Time
	End       time.Time
}

// Meeting - бронь встречи. Status: scheduled либо cancelled.
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

// DecisionInput - снимок кейса на момент хода. Now - момент хода, его задаёт
// вызывающий: тесты и eval передают фиксированное время.
type DecisionInput struct {
	Case                  Case
	Lead                  Lead
	Messages              []Message
	LastIncomingMessageID int64
	OfferedSlots          []OfferedSlot
	// Meeting - активная бронь кейса; nil, если брони нет. ManagerName - имя
	// менеджера брони из каталога, его заполняет dialog.
	Meeting     *Meeting
	ManagerName string
	Now         time.Time
}

// LeadBlock - реплики лида, на которые отвечает ход: все входящие после
// последнего исходящего.
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

// CompleteIsIdempotent - повторный complete не пересчитывает гейт: окна лид
// уже получил. Поправка факта в ready_to_decide это отменяет - изменившийся
// факт обязан пройти гейт заново. Назначенную встречу гейт не пересматривает:
// он не снимает бронь, и лид остался бы с отказом поверх записи.
func CompleteIsIdempotent(status Status, factsChanged bool) bool {
	return status == StatusMeetingScheduled || (status == StatusReadyToDecide && !factsChanged)
}
