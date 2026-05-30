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

type File struct {
	Name string
	Path string
	Size int64
}
