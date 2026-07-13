package chat

import (
	"context"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/auth"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/matches"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

type UserProvider interface {
	Me(context.Context, primitive.ObjectID) (auth.User, error)
}

type MatchProvider interface {
	GetHomeMatches(context.Context) []matches.Match
	GetMatchByID(context.Context, string) (*matches.Match, error)
}

type EventPublisher interface {
	Publish(topic string, data any)
}

type Service struct {
	repo         Repository
	users        UserProvider
	matches      MatchProvider
	publisher    EventPublisher
	sendLimits   *activityLimiter
	reportLimits *activityLimiter
}

func NewService(repo Repository, users UserProvider, matchProvider MatchProvider, publisher EventPublisher) *Service {
	return &Service{
		repo: repo, users: users, matches: matchProvider, publisher: publisher,
		sendLimits: newActivityLimiter(time.Minute), reportLimits: newActivityLimiter(time.Hour),
	}
}

func (s *Service) EnsureIndexes(ctx context.Context) error { return s.repo.EnsureIndexes(ctx) }

type room struct {
	ID, Kind, Title, Status string
	Writable                bool
}

func (s *Service) resolveRoom(ctx context.Context, raw string) (room, *DomainError) {
	raw = strings.TrimSpace(raw)
	if raw == GlobalRoomID {
		return room{ID: GlobalRoomID, Kind: "global", Title: "Global Chat", Writable: true}, nil
	}
	match, err := s.matches.GetMatchByID(ctx, raw)
	if err != nil || match == nil {
		return room{}, errRoomNotFound
	}
	status := matches.NormalizeStatus(match.Status)
	writable := status == matches.StatusUpcoming || status == matches.StatusLive || status == matches.StatusInningsBreak
	return room{
		ID: match.ID.Hex(), Kind: "match", Title: strings.TrimSpace(match.TeamAName + " vs " + match.TeamBName),
		Status: status, Writable: writable,
	}, nil
}

func (s *Service) ListRooms(ctx context.Context, userID primitive.ObjectID) ([]RoomResponse, error) {
	states, err := s.repo.ReadStates(ctx, userID)
	if err != nil {
		return nil, err
	}
	rooms := []room{{ID: GlobalRoomID, Kind: "global", Title: "Global Chat", Writable: true}}
	for _, match := range s.matches.GetHomeMatches(ctx) {
		resolved, roomErr := s.resolveRoom(ctx, match.ID.Hex())
		if roomErr == nil {
			rooms = append(rooms, resolved)
		}
	}
	responses := make([]RoomResponse, 0, len(rooms))
	for _, current := range rooms {
		latest, err := s.repo.LatestMessage(ctx, current.ID)
		if err != nil {
			return nil, err
		}
		unread, err := s.repo.UnreadCount(ctx, current.ID, userID, states[current.ID])
		if err != nil {
			return nil, err
		}
		response := RoomResponse{ID: current.ID, Kind: current.Kind, Title: current.Title, MatchStatus: current.Status, Writable: current.Writable, UnreadCount: unread}
		if latest != nil {
			at := latest.CreatedAt
			response.LatestMessageAt = &at
		}
		responses = append(responses, response)
	}
	sort.SliceStable(responses[1:], func(i, j int) bool {
		a, b := responses[i+1], responses[j+1]
		return roomPriority(a.MatchStatus) < roomPriority(b.MatchStatus)
	})
	return responses, nil
}

func roomPriority(status string) int {
	switch status {
	case matches.StatusLive, matches.StatusInningsBreak:
		return 0
	case matches.StatusUpcoming:
		return 1
	default:
		return 2
	}
}

func (s *Service) ListMessages(ctx context.Context, rawRoomID, cursor string, limit int) (MessagePage, *DomainError, error) {
	resolved, domainErr := s.resolveRoom(ctx, rawRoomID)
	if domainErr != nil {
		return MessagePage{}, domainErr, nil
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 100 {
		limit = 100
	}
	messages, next, err := s.repo.ListMessages(ctx, resolved.ID, strings.TrimSpace(cursor), limit)
	if err != nil {
		if cursor != "" {
			return MessagePage{}, domainError(400, "INVALID_CURSOR", "Invalid pagination cursor"), nil
		}
		return MessagePage{}, nil, err
	}
	items := make([]MessageResponse, 0, len(messages))
	for _, message := range messages {
		items = append(items, messageResponse(message))
	}
	return MessagePage{Items: items, NextCursor: next}, nil, nil
}

var uuidPattern = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func normalizeMessage(raw string) (string, bool) {
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	raw = strings.ReplaceAll(raw, "\r", "\n")
	raw = strings.TrimSpace(raw)
	count := utf8.RuneCountInString(raw)
	if count < 1 || count > MaxMessageRunes {
		return "", false
	}
	for _, r := range raw {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return "", false
		}
	}
	return raw, true
}

func (s *Service) CreateMessage(ctx context.Context, rawRoomID string, userID primitive.ObjectID, clientID, text string) (MessageResponse, bool, *DomainError, error) {
	if !uuidPattern.MatchString(strings.TrimSpace(clientID)) {
		return MessageResponse{}, false, errInvalidClientID, nil
	}
	if existing, found, err := s.repo.FindMessageByClientID(ctx, userID, clientID); err != nil {
		return MessageResponse{}, false, nil, err
	} else if found {
		return messageResponse(existing), false, nil, nil
	}
	resolved, domainErr := s.resolveRoom(ctx, rawRoomID)
	if domainErr != nil {
		return MessageResponse{}, false, domainErr, nil
	}
	if !resolved.Writable {
		return MessageResponse{}, false, errRoomReadOnly, nil
	}
	normalized, ok := normalizeMessage(text)
	if !ok {
		return MessageResponse{}, false, errInvalidMessage, nil
	}
	if retry, allowed := s.sendLimits.allow(userID.Hex(), time.Now(), []limitRule{{5, 10 * time.Second}, {30, time.Minute}}); !allowed {
		err := domainError(429, "CHAT_RATE_LIMITED", "You are sending messages too quickly")
		err.RetryAfter = retry
		return MessageResponse{}, false, err, nil
	}
	user, err := s.users.Me(ctx, userID)
	if err != nil {
		return MessageResponse{}, false, nil, err
	}
	now := time.Now().UTC()
	message := Message{
		ID: primitive.NewObjectID(), RoomID: resolved.ID,
		Author: AuthorSnapshot{ID: user.ID, Name: user.Name, Tier: user.Tier, Role: user.Role},
		Text:   normalized, ClientMessageID: clientID, CreatedAt: now,
	}
	createdMessage, created, err := s.repo.CreateMessage(ctx, message)
	if err != nil {
		return MessageResponse{}, false, nil, err
	}
	response := messageResponse(createdMessage)
	if created && s.publisher != nil {
		s.publisher.Publish(ChatRoomTopic(resolved.ID), ChatEvent{Type: "message.created", Message: response})
	}
	return response, created, nil, nil
}

func ChatRoomTopic(roomID string) string { return "chat:room:" + strings.TrimSpace(roomID) }

func (s *Service) MarkRead(ctx context.Context, rawRoomID string, userID primitive.ObjectID, messageID string) *DomainError {
	resolved, domainErr := s.resolveRoom(ctx, rawRoomID)
	if domainErr != nil {
		return domainErr
	}
	id, err := primitive.ObjectIDFromHex(strings.TrimSpace(messageID))
	if err != nil {
		return errMessageNotFound
	}
	message, found, err := s.repo.FindMessage(ctx, id)
	if err != nil || !found || message.RoomID != resolved.ID {
		return errMessageNotFound
	}
	if err := s.repo.MarkRead(ctx, userID, resolved.ID, message.CreatedAt); err != nil {
		return domainError(500, "CHAT_READ_FAILED", "Unable to update read state")
	}
	return nil
}

func (s *Service) DeleteMessage(ctx context.Context, messageID string, userID primitive.ObjectID, role string) (MessageResponse, bool, *DomainError, error) {
	return s.deleteMessage(ctx, messageID, userID, role, "message_deleted")
}

func (s *Service) deleteMessage(ctx context.Context, messageID string, userID primitive.ObjectID, role, reportResolution string) (MessageResponse, bool, *DomainError, error) {
	id, err := primitive.ObjectIDFromHex(strings.TrimSpace(messageID))
	if err != nil {
		return MessageResponse{}, false, errMessageNotFound, nil
	}
	message, found, err := s.repo.FindMessage(ctx, id)
	if err != nil {
		return MessageResponse{}, false, nil, err
	}
	if !found {
		return MessageResponse{}, false, errMessageNotFound, nil
	}
	if role != "admin" && message.Author.ID != userID {
		return MessageResponse{}, false, errMessageForbidden, nil
	}
	deleted, changed, err := s.repo.MarkDeleted(ctx, id, userID, role)
	if err != nil {
		return MessageResponse{}, false, nil, err
	}
	response := messageResponse(deleted)
	if changed {
		if err := s.repo.ResolveReportsForMessage(ctx, id, userID, reportResolution); err != nil {
			return MessageResponse{}, false, nil, err
		}
		if s.publisher != nil {
			s.publisher.Publish(ChatRoomTopic(message.RoomID), ChatEvent{Type: "message.deleted", Message: response})
		}
	}
	return response, changed, nil, nil
}

var reportReasons = map[string]struct{}{"spam": {}, "abuse": {}, "harassment": {}, "misinformation": {}, "other": {}}

func (s *Service) ReportMessage(ctx context.Context, messageID string, reporterID primitive.ObjectID, reason, note string) (ReportResponse, bool, *DomainError, error) {
	id, err := primitive.ObjectIDFromHex(strings.TrimSpace(messageID))
	if err != nil {
		return ReportResponse{}, false, errMessageNotFound, nil
	}
	reason = strings.ToLower(strings.TrimSpace(reason))
	note = strings.TrimSpace(note)
	if _, ok := reportReasons[reason]; !ok || utf8.RuneCountInString(note) > MaxReportNoteRunes {
		return ReportResponse{}, false, errInvalidReport, nil
	}
	message, found, err := s.repo.FindMessage(ctx, id)
	if err != nil {
		return ReportResponse{}, false, nil, err
	}
	if !found {
		return ReportResponse{}, false, errMessageNotFound, nil
	}
	if message.DeletedAt != nil || message.Author.ID == reporterID {
		return ReportResponse{}, false, errReportForbidden, nil
	}
	if retry, allowed := s.reportLimits.allow(reporterID.Hex(), time.Now(), []limitRule{{10, time.Hour}}); !allowed {
		err := domainError(429, "REPORT_RATE_LIMITED", "Too many reports submitted")
		err.RetryAfter = retry
		return ReportResponse{}, false, err, nil
	}
	report := Report{
		ID: primitive.NewObjectID(), MessageID: message.ID, RoomID: message.RoomID,
		MessageText: message.Text, MessageAuthor: message.Author, ReporterID: reporterID,
		Reason: reason, Note: note, Status: "open", CreatedAt: time.Now().UTC(),
	}
	createdReport, created, err := s.repo.CreateReport(ctx, report)
	if err != nil {
		return ReportResponse{}, false, nil, err
	}
	if !created {
		return ReportResponse{}, false, errReportExists, nil
	}
	return reportResponse(createdReport), created, nil, nil
}

func (s *Service) ListReports(ctx context.Context, status, cursor string, limit int) (ReportPage, *DomainError, error) {
	status = strings.ToLower(strings.TrimSpace(status))
	if status == "" {
		status = "open"
	}
	if status != "open" && status != "resolved" {
		return ReportPage{}, domainError(400, "INVALID_REPORT_STATUS", "Report status must be open or resolved"), nil
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 100 {
		limit = 100
	}
	reports, next, err := s.repo.ListReports(ctx, status, cursor, limit)
	if err != nil {
		if cursor != "" {
			return ReportPage{}, domainError(400, "INVALID_CURSOR", "Invalid pagination cursor"), nil
		}
		return ReportPage{}, nil, err
	}
	items := make([]ReportResponse, 0, len(reports))
	for _, report := range reports {
		items = append(items, reportResponse(report))
	}
	return ReportPage{Items: items, NextCursor: next}, nil, nil
}

func (s *Service) ResolveReport(ctx context.Context, reportID string, adminID primitive.ObjectID, action string) (ReportResponse, *DomainError, error) {
	id, err := primitive.ObjectIDFromHex(strings.TrimSpace(reportID))
	if err != nil {
		return ReportResponse{}, errReportNotFound, nil
	}
	action = strings.ToLower(strings.TrimSpace(action))
	if action != "dismiss" && action != "delete_message" {
		return ReportResponse{}, errInvalidResolution, nil
	}
	report, found, err := s.repo.FindReport(ctx, id)
	if err != nil {
		return ReportResponse{}, nil, err
	}
	if !found {
		return ReportResponse{}, errReportNotFound, nil
	}
	if action == "delete_message" {
		if _, _, domainErr, err := s.deleteMessage(ctx, report.MessageID.Hex(), adminID, "admin", action); err != nil || domainErr != nil && domainErr != errMessageNotFound {
			return ReportResponse{}, domainErr, err
		}
		resolved, _, err := s.repo.FindReport(ctx, id)
		return reportResponse(resolved), nil, err
	}
	resolved, _, err := s.repo.ResolveReport(ctx, id, adminID, action)
	if err != nil {
		return ReportResponse{}, nil, err
	}
	return reportResponse(resolved), nil, nil
}

type limitRule struct {
	max    int
	window time.Duration
}

type activityLimiter struct {
	mu        sync.Mutex
	retention time.Duration
	events    map[string][]time.Time
}

func newActivityLimiter(retention time.Duration) *activityLimiter {
	return &activityLimiter{retention: retention, events: make(map[string][]time.Time)}
}

func (l *activityLimiter) allow(key string, now time.Time, rules []limitRule) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := now.Add(-l.retention)
	existing := l.events[key]
	kept := existing[:0]
	for _, at := range existing {
		if at.After(cutoff) {
			kept = append(kept, at)
		}
	}
	for _, rule := range rules {
		windowStart := now.Add(-rule.window)
		count := 0
		var oldest time.Time
		for _, at := range kept {
			if at.After(windowStart) {
				if oldest.IsZero() {
					oldest = at
				}
				count++
			}
		}
		if count >= rule.max {
			retry := oldest.Add(rule.window).Sub(now)
			if retry < time.Second {
				retry = time.Second
			}
			l.events[key] = kept
			return retry, false
		}
	}
	l.events[key] = append(kept, now)
	return 0, true
}
