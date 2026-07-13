package chat

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type Repository interface {
	EnsureIndexes(context.Context) error
	CreateMessage(context.Context, Message) (Message, bool, error)
	FindMessageByClientID(context.Context, primitive.ObjectID, string) (Message, bool, error)
	FindMessage(context.Context, primitive.ObjectID) (Message, bool, error)
	ListMessages(context.Context, string, string, int) ([]Message, string, error)
	LatestMessage(context.Context, string) (*Message, error)
	MarkDeleted(context.Context, primitive.ObjectID, primitive.ObjectID, string) (Message, bool, error)
	ReadStates(context.Context, primitive.ObjectID) (map[string]time.Time, error)
	MarkRead(context.Context, primitive.ObjectID, string, time.Time) error
	UnreadCount(context.Context, string, primitive.ObjectID, time.Time) (int64, error)
	CreateReport(context.Context, Report) (Report, bool, error)
	FindReport(context.Context, primitive.ObjectID) (Report, bool, error)
	ListReports(context.Context, string, string, int) ([]Report, string, error)
	ResolveReport(context.Context, primitive.ObjectID, primitive.ObjectID, string) (Report, bool, error)
	ResolveReportsForMessage(context.Context, primitive.ObjectID, primitive.ObjectID, string) error
}

func (r *MongoRepository) FindMessageByClientID(ctx context.Context, authorID primitive.ObjectID, clientID string) (Message, bool, error) {
	ctx, cancel := shortContext(ctx)
	defer cancel()
	var message Message
	err := r.messages.FindOne(ctx, bson.M{"author.id": authorID, "clientMessageId": clientID}).Decode(&message)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return Message{}, false, nil
	}
	return message, err == nil, err
}

type MongoRepository struct {
	messages *mongo.Collection
	reads    *mongo.Collection
	reports  *mongo.Collection
}

func NewMongoRepository(db *mongo.Database) *MongoRepository {
	return &MongoRepository{
		messages: db.Collection("chat_messages"),
		reads:    db.Collection("chat_reads"),
		reports:  db.Collection("chat_reports"),
	}
}

func (r *MongoRepository) EnsureIndexes(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := r.messages.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "roomId", Value: 1}, {Key: "createdAt", Value: -1}, {Key: "_id", Value: -1}}},
		{Keys: bson.D{{Key: "author.id", Value: 1}, {Key: "clientMessageId", Value: 1}}, Options: options.Index().SetUnique(true)},
	}); err != nil {
		return err
	}
	if _, err := r.reads.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key: "userId", Value: 1}, {Key: "roomId", Value: 1}}, Options: options.Index().SetUnique(true),
	}); err != nil {
		return err
	}
	_, err := r.reports.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "messageId", Value: 1}, {Key: "reporterId", Value: 1}}, Options: options.Index().SetUnique(true)},
		{Keys: bson.D{{Key: "status", Value: 1}, {Key: "createdAt", Value: -1}, {Key: "_id", Value: -1}}},
	})
	return err
}

func (r *MongoRepository) CreateMessage(ctx context.Context, message Message) (Message, bool, error) {
	ctx, cancel := shortContext(ctx)
	defer cancel()
	_, err := r.messages.InsertOne(ctx, message)
	if err == nil {
		return message, true, nil
	}
	if !mongo.IsDuplicateKeyError(err) {
		return Message{}, false, err
	}
	var existing Message
	err = r.messages.FindOne(ctx, bson.M{"author.id": message.Author.ID, "clientMessageId": message.ClientMessageID}).Decode(&existing)
	return existing, false, err
}

func (r *MongoRepository) FindMessage(ctx context.Context, id primitive.ObjectID) (Message, bool, error) {
	ctx, cancel := shortContext(ctx)
	defer cancel()
	var message Message
	err := r.messages.FindOne(ctx, bson.M{"_id": id}).Decode(&message)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return Message{}, false, nil
	}
	return message, err == nil, err
}

type cursorValue struct {
	CreatedAt time.Time `json:"t"`
	ID        string    `json:"id"`
}

func encodeCursor(createdAt time.Time, id primitive.ObjectID) string {
	payload, _ := json.Marshal(cursorValue{CreatedAt: createdAt.UTC(), ID: id.Hex()})
	return base64.RawURLEncoding.EncodeToString(payload)
}

func decodeCursor(raw string) (time.Time, primitive.ObjectID, error) {
	payload, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return time.Time{}, primitive.NilObjectID, err
	}
	var value cursorValue
	if err := json.Unmarshal(payload, &value); err != nil {
		return time.Time{}, primitive.NilObjectID, err
	}
	id, err := primitive.ObjectIDFromHex(value.ID)
	return value.CreatedAt, id, err
}

func cursorFilter(raw string) (bson.M, error) {
	if raw == "" {
		return bson.M{}, nil
	}
	createdAt, id, err := decodeCursor(raw)
	if err != nil {
		return nil, err
	}
	return bson.M{"$or": bson.A{
		bson.M{"createdAt": bson.M{"$lt": createdAt}},
		bson.M{"createdAt": createdAt, "_id": bson.M{"$lt": id}},
	}}, nil
}

func (r *MongoRepository) ListMessages(ctx context.Context, roomID, cursor string, limit int) ([]Message, string, error) {
	filter, err := cursorFilter(cursor)
	if err != nil {
		return nil, "", err
	}
	filter["roomId"] = roomID
	ctx, cancel := shortContext(ctx)
	defer cancel()
	cur, err := r.messages.Find(ctx, filter, options.Find().SetSort(bson.D{{Key: "createdAt", Value: -1}, {Key: "_id", Value: -1}}).SetLimit(int64(limit+1)))
	if err != nil {
		return nil, "", err
	}
	defer cur.Close(ctx)
	var messages []Message
	if err := cur.All(ctx, &messages); err != nil {
		return nil, "", err
	}
	next := ""
	if len(messages) > limit {
		messages = messages[:limit]
		last := messages[len(messages)-1]
		next = encodeCursor(last.CreatedAt, last.ID)
	}
	return messages, next, nil
}

func (r *MongoRepository) LatestMessage(ctx context.Context, roomID string) (*Message, error) {
	ctx, cancel := shortContext(ctx)
	defer cancel()
	var message Message
	err := r.messages.FindOne(ctx, bson.M{"roomId": roomID}, options.FindOne().SetSort(bson.D{{Key: "createdAt", Value: -1}, {Key: "_id", Value: -1}})).Decode(&message)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, nil
	}
	return &message, err
}

func (r *MongoRepository) MarkDeleted(ctx context.Context, id, deletedBy primitive.ObjectID, role string) (Message, bool, error) {
	ctx, cancel := shortContext(ctx)
	defer cancel()
	now := time.Now().UTC()
	result := r.messages.FindOneAndUpdate(ctx,
		bson.M{"_id": id, "deletedAt": bson.M{"$exists": false}},
		bson.M{"$set": bson.M{"deletedAt": now, "deletedBy": deletedBy, "deletedByRole": role}},
		options.FindOneAndUpdate().SetReturnDocument(options.After),
	)
	var message Message
	if err := result.Decode(&message); err != nil {
		if !errors.Is(err, mongo.ErrNoDocuments) {
			return Message{}, false, err
		}
		message, found, findErr := r.FindMessage(ctx, id)
		return message, false, chooseError(found, findErr)
	}
	return message, true, nil
}

func chooseError(found bool, err error) error {
	if err != nil || found {
		return err
	}
	return mongo.ErrNoDocuments
}

func (r *MongoRepository) ReadStates(ctx context.Context, userID primitive.ObjectID) (map[string]time.Time, error) {
	ctx, cancel := shortContext(ctx)
	defer cancel()
	cur, err := r.reads.Find(ctx, bson.M{"userId": userID})
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	states := map[string]time.Time{}
	for cur.Next(ctx) {
		var state ReadState
		if err := cur.Decode(&state); err != nil {
			return nil, err
		}
		states[state.RoomID] = state.LastReadAt
	}
	return states, cur.Err()
}

func (r *MongoRepository) MarkRead(ctx context.Context, userID primitive.ObjectID, roomID string, at time.Time) error {
	ctx, cancel := shortContext(ctx)
	defer cancel()
	_, err := r.reads.UpdateOne(ctx,
		bson.M{"userId": userID, "roomId": roomID},
		bson.M{"$max": bson.M{"lastReadAt": at.UTC()}, "$setOnInsert": bson.M{"userId": userID, "roomId": roomID}},
		options.Update().SetUpsert(true),
	)
	return err
}

func (r *MongoRepository) UnreadCount(ctx context.Context, roomID string, userID primitive.ObjectID, after time.Time) (int64, error) {
	ctx, cancel := shortContext(ctx)
	defer cancel()
	return r.messages.CountDocuments(ctx, bson.M{
		"roomId": roomID, "createdAt": bson.M{"$gt": after}, "author.id": bson.M{"$ne": userID}, "deletedAt": bson.M{"$exists": false},
	})
}

func (r *MongoRepository) CreateReport(ctx context.Context, report Report) (Report, bool, error) {
	ctx, cancel := shortContext(ctx)
	defer cancel()
	_, err := r.reports.InsertOne(ctx, report)
	if err == nil {
		return report, true, nil
	}
	if !mongo.IsDuplicateKeyError(err) {
		return Report{}, false, err
	}
	var existing Report
	err = r.reports.FindOne(ctx, bson.M{"messageId": report.MessageID, "reporterId": report.ReporterID}).Decode(&existing)
	return existing, false, err
}

func (r *MongoRepository) FindReport(ctx context.Context, id primitive.ObjectID) (Report, bool, error) {
	ctx, cancel := shortContext(ctx)
	defer cancel()
	var report Report
	err := r.reports.FindOne(ctx, bson.M{"_id": id}).Decode(&report)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return Report{}, false, nil
	}
	return report, err == nil, err
}

func (r *MongoRepository) ListReports(ctx context.Context, status, cursor string, limit int) ([]Report, string, error) {
	filter, err := cursorFilter(cursor)
	if err != nil {
		return nil, "", err
	}
	if status != "" {
		filter["status"] = status
	}
	ctx, cancel := shortContext(ctx)
	defer cancel()
	cur, err := r.reports.Find(ctx, filter, options.Find().SetSort(bson.D{{Key: "createdAt", Value: -1}, {Key: "_id", Value: -1}}).SetLimit(int64(limit+1)))
	if err != nil {
		return nil, "", err
	}
	defer cur.Close(ctx)
	var reports []Report
	if err := cur.All(ctx, &reports); err != nil {
		return nil, "", err
	}
	next := ""
	if len(reports) > limit {
		reports = reports[:limit]
		last := reports[len(reports)-1]
		next = encodeCursor(last.CreatedAt, last.ID)
	}
	return reports, next, nil
}

func (r *MongoRepository) ResolveReport(ctx context.Context, id, adminID primitive.ObjectID, resolution string) (Report, bool, error) {
	ctx, cancel := shortContext(ctx)
	defer cancel()
	now := time.Now().UTC()
	result := r.reports.FindOneAndUpdate(ctx, bson.M{"_id": id, "status": "open"}, bson.M{"$set": bson.M{
		"status": "resolved", "resolvedAt": now, "resolvedBy": adminID, "resolution": resolution,
	}}, options.FindOneAndUpdate().SetReturnDocument(options.After))
	var report Report
	if err := result.Decode(&report); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return r.FindReport(ctx, id)
		}
		return Report{}, false, err
	}
	return report, true, nil
}

func (r *MongoRepository) ResolveReportsForMessage(ctx context.Context, messageID, adminID primitive.ObjectID, resolution string) error {
	ctx, cancel := shortContext(ctx)
	defer cancel()
	now := time.Now().UTC()
	_, err := r.reports.UpdateMany(ctx, bson.M{"messageId": messageID, "status": "open"}, bson.M{"$set": bson.M{
		"status": "resolved", "resolvedAt": now, "resolvedBy": adminID, "resolution": resolution,
	}})
	return err
}

func shortContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, 5*time.Second)
}
