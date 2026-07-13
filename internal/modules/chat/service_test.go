package chat

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/auth"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/matches"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

var (
	liveMatchID      = primitive.NewObjectID()
	completedMatchID = primitive.NewObjectID()
	userOneID        = primitive.NewObjectID()
	userTwoID        = primitive.NewObjectID()
	adminID          = primitive.NewObjectID()
)

type fakeUsers map[primitive.ObjectID]auth.User

func (f fakeUsers) Me(_ context.Context, id primitive.ObjectID) (auth.User, error) {
	user, ok := f[id]
	if !ok {
		return auth.User{}, errors.New("user not found")
	}
	return user, nil
}

type fakeMatches struct{ matches []matches.Match }

func (f fakeMatches) GetHomeMatches(context.Context) []matches.Match { return f.matches }
func (f fakeMatches) GetMatchByID(_ context.Context, raw string) (*matches.Match, error) {
	if raw == "1" {
		raw = liveMatchID.Hex()
	}
	if raw == "2" {
		raw = completedMatchID.Hex()
	}
	for i := range f.matches {
		if f.matches[i].ID.Hex() == raw {
			copy := f.matches[i]
			return &copy, nil
		}
	}
	return nil, errors.New("not found")
}

type fakePublisher struct {
	mu     sync.Mutex
	events []ChatEvent
}

func (f *fakePublisher) Publish(_ string, data any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if event, ok := data.(ChatEvent); ok {
		f.events = append(f.events, event)
	}
}

type fakeRepository struct {
	mu       sync.Mutex
	messages map[primitive.ObjectID]Message
	clients  map[string]primitive.ObjectID
	reads    map[string]time.Time
	reports  map[primitive.ObjectID]Report
}

func newFakeRepository() *fakeRepository {
	return &fakeRepository{
		messages: map[primitive.ObjectID]Message{}, clients: map[string]primitive.ObjectID{},
		reads: map[string]time.Time{}, reports: map[primitive.ObjectID]Report{},
	}
}

func (f *fakeRepository) EnsureIndexes(context.Context) error { return nil }
func (f *fakeRepository) CreateMessage(_ context.Context, message Message) (Message, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := message.Author.ID.Hex() + ":" + message.ClientMessageID
	if id, ok := f.clients[key]; ok {
		return f.messages[id], false, nil
	}
	f.messages[message.ID] = message
	f.clients[key] = message.ID
	return message, true, nil
}
func (f *fakeRepository) FindMessageByClientID(_ context.Context, authorID primitive.ObjectID, clientID string) (Message, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.clients[authorID.Hex()+":"+clientID]
	return f.messages[id], ok, nil
}
func (f *fakeRepository) FindMessage(_ context.Context, id primitive.ObjectID) (Message, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	message, ok := f.messages[id]
	return message, ok, nil
}
func (f *fakeRepository) ListMessages(_ context.Context, roomID, cursor string, limit int) ([]Message, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var messages []Message
	for _, message := range f.messages {
		if message.RoomID == roomID {
			messages = append(messages, message)
		}
	}
	sort.Slice(messages, func(i, j int) bool { return messages[i].CreatedAt.After(messages[j].CreatedAt) })
	if cursor != "" {
		at, id, err := decodeCursor(cursor)
		if err != nil {
			return nil, "", err
		}
		filtered := messages[:0]
		for _, message := range messages {
			if message.CreatedAt.Before(at) || message.CreatedAt.Equal(at) && message.ID.Hex() < id.Hex() {
				filtered = append(filtered, message)
			}
		}
		messages = filtered
	}
	next := ""
	if len(messages) > limit {
		messages = messages[:limit]
		last := messages[len(messages)-1]
		next = encodeCursor(last.CreatedAt, last.ID)
	}
	return messages, next, nil
}
func (f *fakeRepository) LatestMessage(_ context.Context, roomID string) (*Message, error) {
	messages, _, _ := f.ListMessages(context.Background(), roomID, "", 1)
	if len(messages) == 0 {
		return nil, nil
	}
	return &messages[0], nil
}
func (f *fakeRepository) MarkDeleted(_ context.Context, id, deletedBy primitive.ObjectID, role string) (Message, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	message, ok := f.messages[id]
	if !ok {
		return Message{}, false, errors.New("not found")
	}
	if message.DeletedAt != nil {
		return message, false, nil
	}
	now := time.Now().UTC()
	message.DeletedAt, message.DeletedBy, message.DeletedByRole = &now, &deletedBy, role
	f.messages[id] = message
	return message, true, nil
}
func readKey(userID primitive.ObjectID, roomID string) string { return userID.Hex() + ":" + roomID }
func (f *fakeRepository) ReadStates(_ context.Context, userID primitive.ObjectID) (map[string]time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	states := map[string]time.Time{}
	for key, at := range f.reads {
		prefix := userID.Hex() + ":"
		if len(key) > len(prefix) && key[:len(prefix)] == prefix {
			states[key[len(prefix):]] = at
		}
	}
	return states, nil
}
func (f *fakeRepository) MarkRead(_ context.Context, userID primitive.ObjectID, roomID string, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := readKey(userID, roomID)
	if current := f.reads[key]; at.After(current) {
		f.reads[key] = at
	}
	return nil
}
func (f *fakeRepository) UnreadCount(_ context.Context, roomID string, userID primitive.ObjectID, after time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var count int64
	for _, message := range f.messages {
		if message.RoomID == roomID && message.Author.ID != userID && message.CreatedAt.After(after) && message.DeletedAt == nil {
			count++
		}
	}
	return count, nil
}
func (f *fakeRepository) CreateReport(_ context.Context, report Report) (Report, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, existing := range f.reports {
		if existing.MessageID == report.MessageID && existing.ReporterID == report.ReporterID {
			return existing, false, nil
		}
	}
	f.reports[report.ID] = report
	return report, true, nil
}
func (f *fakeRepository) FindReport(_ context.Context, id primitive.ObjectID) (Report, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	report, ok := f.reports[id]
	return report, ok, nil
}
func (f *fakeRepository) ListReports(_ context.Context, status, _ string, _ int) ([]Report, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var reports []Report
	for _, report := range f.reports {
		if report.Status == status {
			reports = append(reports, report)
		}
	}
	return reports, "", nil
}
func (f *fakeRepository) ResolveReport(_ context.Context, id, adminID primitive.ObjectID, resolution string) (Report, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	report, ok := f.reports[id]
	if !ok {
		return Report{}, false, nil
	}
	now := time.Now().UTC()
	report.Status, report.Resolution, report.ResolvedAt, report.ResolvedBy = "resolved", resolution, &now, &adminID
	f.reports[id] = report
	return report, true, nil
}
func (f *fakeRepository) ResolveReportsForMessage(_ context.Context, messageID, adminID primitive.ObjectID, resolution string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, report := range f.reports {
		if report.MessageID == messageID && report.Status == "open" {
			now := time.Now().UTC()
			report.Status, report.Resolution, report.ResolvedAt, report.ResolvedBy = "resolved", resolution, &now, &adminID
			f.reports[id] = report
		}
	}
	return nil
}

func newTestService() (*Service, *fakeRepository, *fakePublisher) {
	repo := newFakeRepository()
	publisher := &fakePublisher{}
	users := fakeUsers{
		userOneID: {ID: userOneID, Name: "One", Tier: "STANDARD", Role: "user"},
		userTwoID: {ID: userTwoID, Name: "Two", Tier: "PRO", Role: "user"},
		adminID:   {ID: adminID, Name: "Admin", Tier: "PRO", Role: "admin"},
	}
	matchProvider := fakeMatches{matches: []matches.Match{
		{ID: liveMatchID, TeamAName: "CSK", TeamBName: "MI", Status: matches.StatusLive},
		{ID: completedMatchID, TeamAName: "RCB", TeamBName: "KKR", Status: matches.StatusCompleted},
	}}
	return NewService(repo, users, matchProvider, publisher), repo, publisher
}

func TestCreateMessageCanonicalIdempotentAndArchived(t *testing.T) {
	service, _, publisher := newTestService()
	clientID := "123e4567-e89b-42d3-a456-426614174000"
	message, created, domainErr, err := service.CreateMessage(context.Background(), "1", userOneID, clientID, "Hello cricket")
	if err != nil || domainErr != nil || !created {
		t.Fatalf("create = created:%v domain:%v err:%v", created, domainErr, err)
	}
	if message.RoomID != liveMatchID.Hex() {
		t.Fatalf("room = %s, want %s", message.RoomID, liveMatchID.Hex())
	}
	_, created, domainErr, err = service.CreateMessage(context.Background(), "1", userOneID, clientID, "ignored retry body")
	if err != nil || domainErr != nil || created {
		t.Fatalf("idempotent retry = created:%v domain:%v err:%v", created, domainErr, err)
	}
	if len(publisher.events) != 1 {
		t.Fatalf("published events = %d, want 1", len(publisher.events))
	}
	_, _, domainErr, err = service.CreateMessage(context.Background(), "2", userOneID, "123e4567-e89b-42d3-a456-426614174001", "post match")
	if err != nil || domainErr == nil || domainErr.Code != "ROOM_READ_ONLY" {
		t.Fatalf("archived send domain=%v err=%v", domainErr, err)
	}
}

func TestUnreadReadReportAndDeletePermissions(t *testing.T) {
	service, _, _ := newTestService()
	message, _, domainErr, err := service.CreateMessage(context.Background(), GlobalRoomID, userOneID, "123e4567-e89b-42d3-a456-426614174010", "hello")
	if err != nil || domainErr != nil {
		t.Fatal(err, domainErr)
	}
	rooms, err := service.ListRooms(context.Background(), userTwoID)
	if err != nil || rooms[0].UnreadCount != 1 {
		t.Fatalf("unread = %d err=%v", rooms[0].UnreadCount, err)
	}
	if domainErr := service.MarkRead(context.Background(), GlobalRoomID, userTwoID, message.ID); domainErr != nil {
		t.Fatal(domainErr)
	}
	rooms, _ = service.ListRooms(context.Background(), userTwoID)
	if rooms[0].UnreadCount != 0 {
		t.Fatalf("unread after read = %d", rooms[0].UnreadCount)
	}
	if _, _, domainErr, _ := service.DeleteMessage(context.Background(), message.ID, userTwoID, "user"); domainErr == nil || domainErr.Code != "MESSAGE_FORBIDDEN" {
		t.Fatalf("delete permission = %v", domainErr)
	}
	report, created, domainErr, err := service.ReportMessage(context.Background(), message.ID, userTwoID, "spam", "duplicate links")
	if err != nil || domainErr != nil || !created || report.Status != "open" {
		t.Fatalf("report = created:%v domain:%v err:%v", created, domainErr, err)
	}
	resolved, domainErr, err := service.ResolveReport(context.Background(), report.ID, adminID, "delete_message")
	if err != nil || domainErr != nil || resolved.Status != "resolved" {
		t.Fatalf("resolve = %+v domain:%v err:%v", resolved, domainErr, err)
	}
	page, domainErr, err := service.ListMessages(context.Background(), GlobalRoomID, "", 50)
	if err != nil || domainErr != nil || len(page.Items) != 1 || !page.Items[0].Deleted || page.Items[0].Text != "" {
		t.Fatalf("tombstone = %+v domain:%v err:%v", page, domainErr, err)
	}
}

func TestMessageRateLimit(t *testing.T) {
	service, _, _ := newTestService()
	for i := 0; i < 5; i++ {
		clientID := "123e4567-e89b-42d3-a456-42661417410" + string(rune('0'+i))
		if _, _, domainErr, err := service.CreateMessage(context.Background(), GlobalRoomID, userOneID, clientID, "message"); err != nil || domainErr != nil {
			t.Fatalf("send %d domain=%v err=%v", i, domainErr, err)
		}
	}
	_, _, domainErr, err := service.CreateMessage(context.Background(), GlobalRoomID, userOneID, "123e4567-e89b-42d3-a456-426614174109", "limited")
	if err != nil || domainErr == nil || domainErr.Code != "CHAT_RATE_LIMITED" || domainErr.RetryAfter <= 0 {
		t.Fatalf("rate limit domain=%v err=%v", domainErr, err)
	}
}

func TestCursorRoundTrip(t *testing.T) {
	at := time.Now().UTC().Truncate(time.Millisecond)
	id := primitive.NewObjectID()
	decodedAt, decodedID, err := decodeCursor(encodeCursor(at, id))
	if err != nil || !decodedAt.Equal(at) || decodedID != id {
		t.Fatalf("cursor round trip at=%v id=%v err=%v", decodedAt, decodedID, err)
	}
}
