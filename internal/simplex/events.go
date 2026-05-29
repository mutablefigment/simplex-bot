package simplex

type Event interface{ isSimplexEvent() }

type ChatItemsEvent struct {
	ContactID    int64
	ItemID       int64
	QuotedItemID int64
	Text         string
	Files        []File
}

type ConnectedEvent struct{}

type DisconnectedEvent struct{ Err error }

func (ChatItemsEvent) isSimplexEvent()    {}
func (ConnectedEvent) isSimplexEvent()    {}
func (DisconnectedEvent) isSimplexEvent() {}

// File is an inbound attachment offered on a received chat item. Path is left
// empty here — the bot chooses the destination and calls ReceiveFile to pull
// the bytes down to it.
type File struct {
	ID   int64
	Name string
	Size int64
}

type Chat struct {
	ContactID int64
	Items     []ChatItem
}

type ChatItem struct {
	ItemID   int64
	ItemLive bool
	Mine     bool
}
