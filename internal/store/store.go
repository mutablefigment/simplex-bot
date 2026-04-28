package store

import (
	"context"
	"time"
)

type LiveMessage struct {
	ItemID    int64
	ContactID int64
	StartedAt time.Time
	Finalised bool
}

type Turn struct {
	ID        int64
	SessionID string
	StartedAt time.Time
	EndedAt   time.Time
	CostUSD   float64
	Status    string // ok | timeout | cancelled | error
	Error     string
}

type Store interface {
	GetSessionID(ctx context.Context) (string, error)
	SetSessionID(ctx context.Context, id string) error
	ClearSession(ctx context.Context) error

	InsertLiveMessage(ctx context.Context, lm LiveMessage) error
	FinaliseLiveMessage(ctx context.Context, itemID int64) error
	UnfinalisedLiveMessages(ctx context.Context) ([]LiveMessage, error)

	InsertTurn(ctx context.Context, t Turn) (int64, error)
	UpdateTurn(ctx context.Context, t Turn) error
	TotalCost(ctx context.Context) (float64, error)

	Close() error
}
