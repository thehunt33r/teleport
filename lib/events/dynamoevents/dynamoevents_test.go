/*
Copyright 2018 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

*/

package dynamoevents

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/dynamodb"
	"github.com/aws/aws-sdk-go/service/dynamodb/dynamodbattribute"
	"github.com/gravitational/teleport"
	apidefaults "github.com/gravitational/teleport/api/defaults"
	"github.com/gravitational/teleport/api/types"
	apievents "github.com/gravitational/teleport/api/types/events"
	"github.com/gravitational/teleport/lib/backend/memory"
	"github.com/gravitational/teleport/lib/events"
	"github.com/gravitational/teleport/lib/events/test"
	"github.com/gravitational/teleport/lib/utils"
	"github.com/stretchr/testify/require"

	"github.com/jonboulle/clockwork"
	"github.com/pborman/uuid"
	"gopkg.in/check.v1"

	"github.com/gravitational/trace"
)

const dynamoDBLargeQueryRetries int = 10

func TestMain(m *testing.M) {
	utils.InitLoggerForTests()
	os.Exit(m.Run())
}

func TestDynamoevents(t *testing.T) { check.TestingT(t) }

type suiteBase struct {
	log *Log
	test.EventsSuite
}

func (s *suiteBase) SetUpSuite(c *check.C) {
	testEnabled := os.Getenv(teleport.AWSRunTests)
	if ok, _ := strconv.ParseBool(testEnabled); !ok {
		c.Skip("Skipping AWS-dependent test suite.")
	}

	backend, err := memory.New(memory.Config{})
	c.Assert(err, check.IsNil)

	fakeClock := clockwork.NewFakeClock()
	log, err := New(context.Background(), Config{
		Region:       "eu-north-1",
		Tablename:    fmt.Sprintf("teleport-test-%v", uuid.New()),
		Clock:        fakeClock,
		UIDGenerator: utils.NewFakeUID(),
	}, backend)
	c.Assert(err, check.IsNil)
	s.log = log
	s.EventsSuite.Log = log
	s.EventsSuite.Clock = fakeClock
	s.EventsSuite.QueryDelay = time.Second * 5
}

func (s *suiteBase) SetUpTest(c *check.C) {
	err := s.log.deleteAllItems(context.Background())
	c.Assert(err, check.IsNil)
}

func (s *suiteBase) TearDownSuite(c *check.C) {
	if s.log != nil {
		if err := s.log.deleteTable(context.Background(), s.log.Tablename, true); err != nil {
			c.Fatalf("Failed to delete table: %#v", trace.DebugReport(err))
		}
	}
}

type DynamoeventsSuite struct {
	suiteBase
}

var _ = check.Suite(&DynamoeventsSuite{})

func (s *DynamoeventsSuite) TestPagination(c *check.C) {
	s.EventPagination(c)
}

var letterRunes = []rune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ")

func randStringAlpha(n int) string {
	b := make([]rune, n)
	for i := range b {
		b[i] = letterRunes[rand.Intn(len(letterRunes))]
	}
	return string(b)
}

func (s *DynamoeventsSuite) TestSizeBreak(c *check.C) {
	const eventSize = 50 * 1024
	blob := randStringAlpha(eventSize)

	const eventCount int = 10
	for i := 0; i < eventCount; i++ {
		err := s.Log.EmitAuditEventLegacy(events.UserLocalLoginE, events.EventFields{
			events.LoginMethod:        events.LoginMethodSAML,
			events.AuthAttemptSuccess: true,
			events.EventUser:          "bob",
			events.EventTime:          s.Clock.Now().UTC().Add(time.Second * time.Duration(i)),
			"test.data":               blob,
		})
		c.Assert(err, check.IsNil)
	}

	var checkpoint string
	events := make([]apievents.AuditEvent, 0)

	for {
		fetched, lCheckpoint, err := s.log.SearchEvents(s.Clock.Now().UTC().Add(-time.Hour), s.Clock.Now().UTC().Add(time.Hour), apidefaults.Namespace, nil, eventCount, types.EventOrderDescending, checkpoint)
		c.Assert(err, check.IsNil)
		checkpoint = lCheckpoint
		events = append(events, fetched...)

		if checkpoint == "" {
			break
		}
	}

	lastTime := s.Clock.Now().UTC().Add(time.Hour)

	for _, event := range events {
		c.Assert(event.GetTime().Before(lastTime), check.Equals, true)
		lastTime = event.GetTime()
	}
}

func (s *DynamoeventsSuite) TestSessionEventsCRUD(c *check.C) {
	s.SessionEventsCRUD(c)
}

// TestIndexExists tests functionality of the `Log.indexExists` function.
func (s *DynamoeventsSuite) TestIndexExists(c *check.C) {
	hasIndex, err := s.log.indexExists(context.Background(), s.log.Tablename, indexTimeSearchV2)
	c.Assert(err, check.IsNil)
	c.Assert(hasIndex, check.Equals, true)
}

// TestDateRangeGenerator tests the `daysBetween` function which generates ISO 6801
// date strings for every day between two points in time.
func TestDateRangeGenerator(t *testing.T) {
	// date range within a month
	start := time.Date(2021, 4, 10, 8, 5, 0, 0, time.UTC)
	end := start.Add(time.Hour * time.Duration(24*4))
	days := daysBetween(start, end)
	require.Equal(t, []string{"2021-04-10", "2021-04-11", "2021-04-12", "2021-04-13", "2021-04-14"}, days)

	// date range transitioning between two months
	start = time.Date(2021, 8, 30, 8, 5, 0, 0, time.UTC)
	end = start.Add(time.Hour * time.Duration(24*2))
	days = daysBetween(start, end)
	require.Equal(t, []string{"2021-08-30", "2021-08-31", "2021-09-01"}, days)
}

func (s *DynamoeventsSuite) TestRFD24Migration(c *check.C) {
	eventTemplate := preRFD24event{
		SessionID:      uuid.New(),
		EventIndex:     -1,
		EventType:      "test.event",
		Fields:         "{}",
		EventNamespace: apidefaults.Namespace,
	}

	for i := 0; i < 10; i++ {
		eventTemplate.EventIndex++
		event := eventTemplate
		event.CreatedAt = time.Date(2021, 4, 10, 8, 5, 0, 0, time.UTC).Add(time.Hour * time.Duration(24*i)).Unix()
		err := s.log.emitTestAuditEvent(context.TODO(), event)
		c.Assert(err, check.IsNil)
	}

	err := s.log.migrateDateAttribute(context.TODO())
	c.Assert(err, check.IsNil)

	start := time.Date(2021, 4, 9, 8, 5, 0, 0, time.UTC)
	end := start.Add(time.Hour * time.Duration(24*11))
	var eventArr []event
	err = utils.RetryStaticFor(time.Minute*5, time.Second*5, func() error {
		eventArr, _, err = s.log.searchEventsRaw(start, end, apidefaults.Namespace, 1000, types.EventOrderAscending, "", searchEventsFilter{eventTypes: []string{"test.event"}})
		return err
	})
	c.Assert(err, check.IsNil)
	c.Assert(eventArr, check.HasLen, 10)

	for _, event := range eventArr {
		dateString := time.Unix(event.CreatedAt, 0).Format(iso8601DateFormat)
		c.Assert(dateString, check.Equals, event.CreatedAtDate)
	}
}

type preRFD24event struct {
	SessionID      string
	EventIndex     int64
	EventType      string
	CreatedAt      int64
	Expires        *int64 `json:"Expires,omitempty"`
	Fields         string
	EventNamespace string
}

func (s *DynamoeventsSuite) TestFieldsMapMigration(c *check.C) {
	ctx := context.Background()
	eventTemplate := eventWithJSONFields{
		SessionID:      uuid.New(),
		EventIndex:     -1,
		EventType:      "test.event",
		Fields:         "{}",
		EventNamespace: apidefaults.Namespace,
	}
	sessionEndFields := events.EventFields{"participants": []interface{}{"test-user1", "test-user2"}}
	sessionEndFieldsJSON, err := json.Marshal(sessionEndFields)
	c.Assert(err, check.IsNil)

	start := time.Date(2021, 4, 9, 8, 5, 0, 0, time.UTC)
	step := 24 * time.Hour
	end := start.Add(20 * step)
	for t := start.Add(time.Minute); t.Before(end); t = t.Add(step) {
		eventTemplate.EventIndex++
		event := eventTemplate
		if t.Day()%3 == 0 {
			event.EventType = events.SessionEndEvent
			event.Fields = string(sessionEndFieldsJSON)
		}
		event.CreatedAt = t.Unix()
		event.CreatedAtDate = t.Format(iso8601DateFormat)
		err := s.log.emitTestAuditEvent(ctx, event)
		c.Assert(err, check.IsNil)
	}

	err = s.log.convertFieldsToDynamoMapFormat(ctx)
	c.Assert(err, check.IsNil)

	eventArr, _, err := s.log.searchEventsRaw(start, end, apidefaults.Namespace, 1000, types.EventOrderAscending, "", searchEventsFilter{})
	c.Assert(err, check.IsNil)

	for _, event := range eventArr {
		fields := events.EventFields{}
		if event.EventType == events.SessionEndEvent {
			fields = sessionEndFields
		}
		c.Assert(event.FieldsMap, check.DeepEquals, fields)
	}
}

type eventWithJSONFields struct {
	SessionID      string
	EventIndex     int64
	EventType      string
	CreatedAt      int64
	Expires        *int64 `json:"Expires,omitempty"`
	Fields         string
	EventNamespace string
	CreatedAtDate  string
}

// emitTestAuditEvent emits a struct as a test audit event.
func (l *Log) emitTestAuditEvent(ctx context.Context, e interface{}) error {
	av, err := dynamodbattribute.MarshalMap(e)
	if err != nil {
		return trace.Wrap(err)
	}
	input := dynamodb.PutItemInput{
		Item:      av,
		TableName: aws.String(l.Tablename),
	}
	_, err = l.svc.PutItemWithContext(ctx, &input)
	return trace.Wrap(convertError(err))
}

type DynamoeventsLargeTableSuite struct {
	suiteBase
}

var _ = check.Suite(&DynamoeventsLargeTableSuite{})

// TestLargeTableRetrieve checks that we can retrieve all items from a large
// table at once. It is run in a separate suite with its own table to avoid the
// prolonged table clearing and the consequent 'test timed out' errors.
func (s *DynamoeventsLargeTableSuite) TestLargeTableRetrieve(c *check.C) {
	const eventCount = 4000
	for i := 0; i < eventCount; i++ {
		err := s.Log.EmitAuditEventLegacy(events.UserLocalLoginE, events.EventFields{
			events.LoginMethod:        events.LoginMethodSAML,
			events.AuthAttemptSuccess: true,
			events.EventUser:          "bob",
			events.EventTime:          s.Clock.Now().UTC(),
		})
		c.Assert(err, check.IsNil)
	}

	var (
		history []apievents.AuditEvent
		err     error
	)
	for i := 0; i < dynamoDBLargeQueryRetries; i++ {
		time.Sleep(s.EventsSuite.QueryDelay)

		history, _, err = s.Log.SearchEvents(s.Clock.Now().Add(-1*time.Hour), s.Clock.Now().Add(time.Hour), apidefaults.Namespace, nil, 0, types.EventOrderAscending, "")
		c.Assert(err, check.IsNil)

		if len(history) == eventCount {
			break
		}
	}

	// `check.HasLen` prints the entire array on failure, which pollutes the output.
	c.Assert(len(history), check.Equals, eventCount)
}

func TestFromWhereExpr(t *testing.T) {
	t.Parallel()

	// !(equals(login, "root") || equals(login, "admin")) && contains(participants, "test-user")
	cond := &types.WhereExpr{And: types.WhereExpr2{
		L: &types.WhereExpr{Not: &types.WhereExpr{Or: types.WhereExpr2{
			L: &types.WhereExpr{Equals: types.WhereExpr2{L: &types.WhereExpr{Field: "login"}, R: &types.WhereExpr{Literal: "root"}}},
			R: &types.WhereExpr{Equals: types.WhereExpr2{L: &types.WhereExpr{Field: "login"}, R: &types.WhereExpr{Literal: "admin"}}},
		}}},
		R: &types.WhereExpr{Contains: types.WhereExpr2{L: &types.WhereExpr{Field: "participants"}, R: &types.WhereExpr{Literal: "test-user"}}},
	}}

	params := condFilterParams{attrNames: map[string]string{}, attrValues: map[string]interface{}{}}
	expr, err := fromWhereExpr(cond, &params)
	require.NoError(t, err)

	require.Equal(t, "(NOT ((FieldsMap.#condName0 = :condValue0) OR (FieldsMap.#condName0 = :condValue1))) AND (contains(FieldsMap.#condName1, :condValue2))", expr)
	require.Equal(t, condFilterParams{
		attrNames:  map[string]string{"#condName0": "login", "#condName1": "participants"},
		attrValues: map[string]interface{}{":condValue0": "root", ":condValue1": "admin", ":condValue2": "test-user"},
	}, params)
}
