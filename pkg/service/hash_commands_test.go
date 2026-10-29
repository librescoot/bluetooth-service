package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/librescoot/bluetooth-service/pkg/ble"
	"github.com/librescoot/bluetooth-service/pkg/logger"
	"github.com/librescoot/bluetooth-service/pkg/usock"
	"github.com/redis/go-redis/v9"
)

func TestParseHashRequest(t *testing.T) {
	for _, command := range []string{
		"read:1234abcd:20:battery:0/present,serial-number",
		"read:1234abcd:244:battery:1/serial-number,present",
		"read:1234abcd:100:battery:0/present",
		"get:1234abcd:35s:20",
	} {
		if req, code := parseHashRequest(command); code != "" || req.id != "1234abcd" {
			t.Errorf("%q: %+v %s", command, req, code)
		}
	}
	for _, tc := range []struct{ command, code string }{
		{"", "command"},
		{"read", "command"},
		{"read:00000000:20:battery:0/present", "command"},
		{"read:1234ABCD:20:battery:0/present", "command"},
		{"read:1234abc:20:battery:0/present", "command"},
		{"read:1234abcde:20:battery:0/present", "command"},
		{"write:1234abcd:20:battery:0/present", "command"},
		{"read:1234abcd:19:battery:0/present", "budget"},
		{"read:1234abcd:245:battery:0/present", "budget"},
		{"read:1234abcd:+20:battery:0/present", "budget"},
		{"read:1234abcd:020:battery:0/present", "budget"},
		{"read:1234abcd:20:battery:0", "command"},
		{"read:1234abcd:20:battery:0/", "command"},
		{"read:1234abcd:20:battery:0/present,present", "command"},
		{"read:1234abcd:20:battery:0/present,", "command"},
		{"read:1234abcd:20:battery:0/present,serial-number,present", "command"},
		{"read:1234abcd:20:battery:0/" + strings.Repeat("x", 68), "denied"}, // exactly 100 bytes with hash:
		{"read:1234abcd:20:battery:0/" + strings.Repeat("x", 69), "command"},
		{"read:1234abcd:20:battery:0/" + strings.Repeat("é", 36), "command"},
		{"read:1234abcd:20:battery:0/\xff", "command"},
		{"get:1234abcd:0:19", "budget"},
		{"get:1234abcd:0:20:extra", "budget"},
		{"get:1234abcd:-1:20", "offset"},
		{"get:1234abcd:00:20", "offset"},
		{"get:1234abcd:Z:20", "offset"},
		{"get:1234abcd:35t:20", "offset"},
		{"get:1234abcd:999999999999999999:20", "offset"},
	} {
		req, code := parseHashRequest(tc.command)
		if code != tc.code {
			t.Errorf("%q: %s, want %s", tc.command, code, tc.code)
		}
		if len("r:x:"+req.id+":"+code) > 20 {
			t.Errorf("oversized error for %q", tc.command)
		}
	}
}

func TestHashReadAllowlist(t *testing.T) {
	s, _ := newVersionPushService(t)
	for _, selector := range []string{
		"settings/present", "system/serial-number", "battery:2/present", "battery:00/present",
		"battery:*/present", "Battery:0/present", "battery:0 /present", "battery%3A0/present",
		"battery:0\x00/present", "battery:0/Present", "battery:0/*", "battery:0/serial_number",
		"battery:0/present,secret", "battery:0/present,serial-number/extra",
		"battery:0/present%2Cserial-number", "battery:0/serial-number\n", "battery:0/serial-number:secret",
	} {
		if got := s.hashResponse("read:1234abcd:20:" + selector); got != "r:x:1234abcd:denied" {
			t.Errorf("%q: %s", selector, got)
		}
	}
	if len(s.hashSnapshots) != 0 {
		t.Fatal("denied requests allocated snapshots")
	}
}

func TestHashReadRawValues(t *testing.T) {
	for _, tc := range []struct {
		name, hash string
		fields     []string
		stored     []string
		want       string
	}{
		{"missing hash", "battery:0", []string{"present", "serial-number"}, nil, `[null,null]`},
		{"missing field", "battery:1", []string{"present", "serial-number"}, []string{"present", "false"}, `["false",null]`},
		{"empty and cached identity", "battery:0", []string{"present", "serial-number"}, []string{"present", "", "serial-number", "000abc\x00"}, `["","000abc\u0000"]`},
		{"no interpretation", "battery:1", []string{"serial-number", "present"}, []string{"present", "unknown", "serial-number", " unnormalized "}, `[" unnormalized ","unknown"]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, mr := newVersionPushService(t)
			if tc.stored != nil {
				mr.HSet(tc.hash, tc.stored...)
			}
			req := hashReadRequest{hash: tc.hash, fields: tc.fields}
			got, code := s.readHashValues(req)
			if code != "" || string(got) != tc.want {
				t.Fatalf("got %q %s, want %q", got, code, tc.want)
			}
		})
	}
}

func TestHashReadErrorsAndValueBounds(t *testing.T) {
	for _, tc := range []struct{ value, code string }{
		{strings.Repeat("x", 256), ""},
		{strings.Repeat("é", 128), ""},
		{strings.Repeat("\x00", 256), ""},
		{strings.Repeat("x", 257), "large"},
		{strings.Repeat("é", 129), "large"},
		{strings.Repeat("x", 1024*1024), "large"},
		{"\xff", "utf8"},
	} {
		s, mr := newVersionPushService(t)
		mr.HSet("battery:0", "present", tc.value, "serial-number", tc.value)
		response := s.hashResponse("read:1234abcd:20:battery:0/present,serial-number")
		if tc.code != "" {
			if response != "r:x:1234abcd:"+tc.code || len(s.hashSnapshots) != 0 {
				t.Fatalf("error response %s; snapshots %d", response, len(s.hashSnapshots))
			}
		} else {
			data := s.hashSnapshots["1234abcd"].data
			if len(data) > hashJSONMaxBytes {
				t.Fatalf("JSON too large: %d", len(data))
			}
			var values []string
			if err := json.Unmarshal(data, &values); err != nil || !reflect.DeepEqual(values, []string{tc.value, tc.value}) {
				t.Fatalf("value changed: %v", err)
			}
		}
	}
	s, mr := newVersionPushService(t)
	mr.Set("battery:0", "not a hash")
	if got := s.hashResponse("read:1234abcd:20:battery:0/present"); got != "r:x:1234abcd:redis" {
		t.Fatal(got)
	}
	s.ipc.Raw().Close()
	if got := s.hashResponse("read:1234abcd:20:battery:1/present"); got != "r:x:1234abcd:redis" {
		t.Fatal(got)
	}
	s.ipc = nil
	if got := s.hashResponse("read:1234abcd:20:battery:1/present"); got != "r:x:1234abcd:redis" {
		t.Fatal(got)
	}
}

func TestHashChunksNotificationBudget(t *testing.T) {
	for _, value := range []string{"", "short", "é😀\x00\"\\", strings.Repeat("\x00", 256), strings.Repeat("😀", 64)} {
		data, _ := json.Marshal([]string{value, value})
		for budget := 20; budget <= 244; budget++ {
			req := hashReadRequest{id: "1234abcd", budget: budget}
			var assembled []byte
			for len(assembled) < len(data) {
				req.offset = len(assembled)
				response := hashChunk(data, req)
				prefix := fmt.Sprintf("r:p:1234abcd:%s:", strconv.FormatInt(int64(req.offset), 36))
				if !strings.HasPrefix(response, prefix) || len(response) > budget {
					t.Fatalf("budget %d: %q", budget, response)
				}
				part, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(response, prefix))
				if err != nil || len(part) == 0 {
					t.Fatalf("empty or invalid chunk: %q", response)
				}
				assembled = append(assembled, part...)
			}
			if !bytes.Equal(assembled, data) {
				t.Fatal("chunk assembly changed data")
			}
			req.offset = len(data)
			if got := hashChunk(data, req); got != "r:e:1234abcd:"+strconv.FormatInt(int64(len(data)), 36) {
				t.Fatal(got)
			}
			req.offset++
			if got := hashChunk(data, req); got != "r:x:1234abcd:offset" {
				t.Fatal(got)
			}
		}
	}
}

func TestHashSnapshotLifecycleAndCorrelation(t *testing.T) {
	s, mr := newVersionPushService(t)
	mr.HSet("battery:0", "present", "first")
	for i := 1; i <= 4; i++ {
		id := fmt.Sprintf("%08x", i)
		if got := s.hashResponse("read:" + id + ":20:battery:0/present"); got != "r:s:"+id+":9" {
			t.Fatal(got)
		}
	}
	if got := s.hashResponse("read:00000005:20:battery:0/present"); got != "r:x:00000005:busy" {
		t.Fatal(got)
	}
	mr.HSet("battery:0", "present", "second")
	if got := s.hashResponse("read:00000001:20:battery:0/present"); got != "r:x:00000001:id" {
		t.Fatal(got)
	}
	if got := s.hashResponse("get:00000001:0:244"); got != "r:p:00000001:0:WyJmaXJzdCJd" {
		t.Fatal(got)
	}
	if got := s.hashResponse("get:00000002:0:244"); got != "r:p:00000002:0:WyJmaXJzdCJd" {
		t.Fatal(got)
	}
	until := s.hashSnapshots["00000001"].until
	if time.Until(until) < 4*time.Minute || time.Until(until) > hashSnapshotTTL {
		t.Fatalf("unexpected TTL %v", time.Until(until))
	}
	if got := s.hashResponse("get:00000001:1:20"); !strings.HasPrefix(got, "r:p:00000001:1:") || s.hashSnapshots["00000001"].until != until {
		t.Fatal("get changed lifetime or offset")
	}
	if got := s.hashResponse("get:00000001:a:20"); got != "r:x:00000001:offset" {
		t.Fatal(got)
	}
	if got := s.hashResponse("get:00000001:9:20"); got != "r:e:00000001:9" {
		t.Fatal(got)
	}
	if got := s.hashResponse("get:00000001:9:20"); got != "r:x:00000001:session" {
		t.Fatal(got)
	}
	if got := s.hashResponse("read:00000005:20:battery:0/present"); got != "r:s:00000005:a" {
		t.Fatal(got)
	}
	s.hashSnapshots["00000002"].until = time.Now()
	if got := s.hashResponse("get:00000002:0:20"); got != "r:x:00000002:session" {
		t.Fatal(got)
	}
	s.handleBLEParamMessage(ble.TypeBLEParam, uint16(ble.TypeBLEStatus), "disconnected")
	if got := s.hashResponse("get:00000003:0:20"); got != "r:x:00000003:session" || len(s.hashSnapshots) != 0 {
		t.Fatal(got)
	}
}

type hashRedisHook struct {
	process func(context.Context, redis.Cmder, redis.ProcessHook) error
}

func (h hashRedisHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h hashRedisHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}
func (h hashRedisHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error { return h.process(ctx, cmd, next) }
}

func TestHashReadAtomicAndReadOnly(t *testing.T) {
	s, mr := newVersionPushService(t)
	mr.HSet("battery:0", "present", "initial", "serial-number", "initial", "secret", "never read")
	var mu sync.Mutex
	var commands [][]interface{}
	s.ipc.Raw().AddHook(hashRedisHook{process: func(ctx context.Context, cmd redis.Cmder, next redis.ProcessHook) error {
		mu.Lock()
		commands = append(commands, append([]interface{}(nil), cmd.Args()...))
		mu.Unlock()
		return next(ctx, cmd)
	}})
	writer := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer writer.Close()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			value := strconv.Itoa(i)
			if err := writer.HSet(context.Background(), "battery:0", "present", value, "serial-number", value).Err(); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	for i := 0; i < 100; i++ {
		data, code := s.readHashValues(hashReadRequest{hash: "battery:0", fields: []string{"present", "serial-number"}})
		var values []string
		if code != "" || json.Unmarshal(data, &values) != nil || len(values) != 2 || values[0] != values[1] {
			t.Errorf("torn read %q %s", data, code)
		}
	}
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if len(commands) != 100 {
		t.Fatalf("%d commands, want exactly one per read", len(commands))
	}
	want := []interface{}{"eval", hashReadScript, 1, "battery:0", hashValueMaxBytes, "present", "serial-number"}
	for _, command := range commands {
		if !reflect.DeepEqual(command, want) {
			t.Fatalf("unexpected Redis command: %v", command)
		}
	}
	if got := mr.HGet("battery:0", "secret"); got != "never read" {
		t.Fatal("unrequested field changed")
	}
}

func TestHashReadDisconnectDuringRead(t *testing.T) {
	s, _ := newVersionPushService(t)
	entered, release := make(chan struct{}), make(chan struct{})
	s.ipc.Raw().AddHook(hashRedisHook{process: func(ctx context.Context, cmd redis.Cmder, next redis.ProcessHook) error {
		if cmd.Name() == "eval" {
			close(entered)
			<-release
		}
		return next(ctx, cmd)
	}})
	result := make(chan string, 1)
	go func() { result <- s.hashResponse("read:1234abcd:20:battery:0/present") }()
	<-entered
	if got := s.hashResponse("read:1234abcd:20:battery:0/present"); got != "r:x:1234abcd:id" {
		t.Fatal(got)
	}
	s.clearHashSnapshots()
	close(release)
	if got := <-result; got != "" || len(s.hashSnapshots) != 0 {
		t.Fatalf("late response after disconnect: %q", got)
	}
}

func TestHashConcurrentRequests(t *testing.T) {
	s, _ := newVersionPushService(t)
	var wg sync.WaitGroup
	for i := 1; i <= 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("%08x", i)
			got := s.hashResponse("read:" + id + ":20:battery:0/present")
			if got != "r:s:"+id+":6" && got != "r:x:"+id+":busy" {
				t.Errorf("cross-request response: %s", got)
			}
		}(i)
	}
	wg.Wait()
	if len(s.hashSnapshots) != 4 {
		t.Fatalf("snapshot limit: %d", len(s.hashSnapshots))
	}
}

func TestHashDispatchCapabilitiesAndRedaction(t *testing.T) {
	s, mr := newVersionPushService(t)
	var output bytes.Buffer
	s.log = logger.NewLogger(log.New(&output, "", 0), logger.LogLevelDebug)
	const secret = "sensitive-identity"
	mr.HSet("battery:0", "serial-number", secret)
	for _, command := range []string{
		"hash:read:1234abcd:244:battery:0/serial-number",
		"hash:get:1234abcd:0:244",
		"hash:read:2345abcd:20:battery:0/" + secret,
	} {
		payload, err := cbor.Marshal(map[uint16]map[uint16]string{
			uint16(ble.TypeExtended): {uint16(ble.TypeExtendedCommand): command},
		})
		if err != nil {
			t.Fatal(err)
		}
		s.HandleUSockMessage(byte(uint16(ble.TypeExtended)&0xff), &usock.Payload{Data: payload})
	}
	logs := output.String()
	for _, private := range []string{secret, "battery:0", "serial-number", "1234abcd", base64.RawURLEncoding.EncodeToString([]byte(`["` + secret + `"]`))} {
		if strings.Contains(logs, private) {
			t.Fatalf("payload leaked: %s", logs)
		}
	}
	if !strings.Contains(logs, "Received extended command category: hash") || !strings.Contains(logs, "Sent extended response category: r") {
		t.Fatal(logs)
	}
	mr.HSet("sms:inbox", "snapshot", `{"version":1,"unread":0,"messages":[]}`)
	if err := s.PublishCapabilities(); err != nil {
		t.Fatal(err)
	}
	registry := mr.HGet("system", "capabilities")
	if !strings.Contains(registry, ":hash=1") || !strings.Contains(registry, ":sms=1") || strings.Contains(registry, "battery=") {
		t.Fatal(registry)
	}
	if _, ok := capabilityMap["battery"]; ok || len(capabilityMap["hash"]) != 2 {
		t.Fatal("unexpected capability map")
	}
	s.handleExtendedCommandMessage(ble.TypeExtended, uint16(ble.TypeExtendedCommand), "battery:snapshot:0")
	if got := routeResponse(t, s.usock.(*mockUSOCK)); got != "error:unknown command" {
		t.Fatal(got)
	}
}

func TestHashContractVectors(t *testing.T) {
	var vectors []struct {
		Name, Hash, Payload, ReadRequest, ReadResponse string
		Fields                                         []string
		Values                                         []*string
		Budget                                         int
		Chunks                                         []struct{ Request, Response string }
	}
	data, err := os.ReadFile("testdata/hash_contract_vectors.json")
	if err != nil || json.Unmarshal(data, &vectors) != nil || len(vectors) == 0 {
		t.Fatalf("contract vectors: %v", err)
	}
	for _, vector := range vectors {
		t.Run(vector.Name, func(t *testing.T) {
			s, mr := newVersionPushService(t)
			for i, value := range vector.Values {
				if value != nil {
					mr.HSet(vector.Hash, vector.Fields[i], *value)
				}
			}
			if got := s.hashResponse(strings.TrimPrefix(vector.ReadRequest, "hash:")); got != vector.ReadResponse {
				t.Fatalf("%s: %s, want %s", vector.ReadRequest, got, vector.ReadResponse)
			}
			var assembled []byte
			for _, chunk := range vector.Chunks {
				got := s.hashResponse(strings.TrimPrefix(chunk.Request, "hash:"))
				if got != chunk.Response || len(got) > vector.Budget {
					t.Fatalf("%s: %s, want %s", chunk.Request, got, chunk.Response)
				}
				if strings.HasPrefix(got, "r:p:") {
					part, err := base64.RawURLEncoding.DecodeString(strings.Split(got, ":")[4])
					if err != nil {
						t.Fatal(err)
					}
					assembled = append(assembled, part...)
				}
			}
			if string(assembled) != vector.Payload {
				t.Fatalf("got payload %q, want %q", assembled, vector.Payload)
			}
		})
	}
}

func TestHashAdmissionIncludesPacedSender(t *testing.T) {
	s, _ := newVersionPushService(t)
	s.extRespMu.Lock()
	var unlock sync.Once
	t.Cleanup(func() { unlock.Do(s.extRespMu.Unlock) })
	var wg sync.WaitGroup
	for i := 0; i < hashHandlerLimit; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.handleHashCommand("malformed")
		}()
	}
	deadline := time.Now().Add(time.Second)
	for s.hashHandlers.Load() != hashHandlerLimit {
		if time.Now().After(deadline) {
			t.Fatal("handlers did not reach paced sender")
		}
		time.Sleep(time.Millisecond)
	}
	dropped := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			s.handleHashCommand("read:1234abcd:20:battery:0/present")
		}
		close(dropped)
	}()
	select {
	case <-dropped:
	case <-time.After(time.Second):
		t.Fatal("overload queued behind sender")
	}
	if s.hashHandlers.Load() != hashHandlerLimit || len(s.hashSnapshots) != 0 {
		t.Fatal("overload changed admission or allocated snapshots")
	}
	unlock.Do(s.extRespMu.Unlock)
	wg.Wait()
	if s.hashHandlers.Load() != 0 || s.usock.(*mockUSOCK).messageCount() != hashHandlerLimit {
		t.Fatal("admission leaked or overload emitted responses")
	}
	s.handleHashCommand("read:1234abcd:20:battery:0/present")
	if got := routeResponse(t, s.usock.(*mockUSOCK)); got != "r:s:1234abcd:6" {
		t.Fatal(got)
	}
}

func TestHashAdmissionSurvivesDisconnectDuringRedisReads(t *testing.T) {
	s, _ := newVersionPushService(t)
	entered := make(chan struct{}, hashHandlerLimit)
	release := make(chan struct{})
	var unblock sync.Once
	t.Cleanup(func() { unblock.Do(func() { close(release) }) })
	s.ipc.Raw().AddHook(hashRedisHook{process: func(ctx context.Context, cmd redis.Cmder, next redis.ProcessHook) error {
		if cmd.Name() == "eval" {
			entered <- struct{}{}
			<-release
		}
		return next(ctx, cmd)
	}})
	var wg sync.WaitGroup
	for i := 1; i <= hashHandlerLimit; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s.handleHashCommand(fmt.Sprintf("read:%08x:20:battery:0/present", i))
		}(i)
	}
	for i := 0; i < hashHandlerLimit; i++ {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("read did not reach Redis")
		}
	}
	s.clearHashSnapshots()
	dropped := make(chan struct{})
	go func() {
		s.handleHashCommand("read:1234abcd:20:battery:0/present")
		close(dropped)
	}()
	select {
	case <-dropped:
	case <-time.After(time.Second):
		t.Fatal("disconnect reset admission while old reads were pending")
	}
	if s.hashHandlers.Load() != hashHandlerLimit || len(s.hashSnapshots) != 0 || len(entered) != 0 {
		t.Fatal("disconnect admitted excess read")
	}
	unblock.Do(func() { close(release) })
	wg.Wait()
	if s.hashHandlers.Load() != 0 || s.usock.(*mockUSOCK).messageCount() != 0 {
		t.Fatal("late reads leaked admission or sent stale responses")
	}
	s.handleHashCommand("read:2345abcd:20:battery:0/present")
	if got := routeResponse(t, s.usock.(*mockUSOCK)); got != "r:s:2345abcd:6" {
		t.Fatal(got)
	}
}

func TestHashWorstCaseSmallMTUPacing(t *testing.T) {
	s, mr := newVersionPushService(t)
	value := strings.Repeat("\x00", hashValueMaxBytes)
	mr.HSet("battery:0", "present", value, "serial-number", value)
	if got := s.hashResponse("read:1234abcd:20:battery:0/present,serial-number"); got != "r:s:1234abcd:2dj" {
		t.Fatal(got)
	}
	var assembled []byte
	packets := 0
	for {
		offset := strconv.FormatInt(int64(len(assembled)), 36)
		response := s.hashResponse("get:1234abcd:" + offset + ":20")
		if len(response) > 20 {
			t.Fatal("notification budget exceeded")
		}
		if response == "r:e:1234abcd:"+offset {
			break
		}
		prefix := "r:p:1234abcd:" + offset + ":"
		if !strings.HasPrefix(response, prefix) {
			t.Fatal(response)
		}
		part, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(response, prefix))
		if err != nil || len(part) == 0 {
			t.Fatal("invalid chunk")
		}
		assembled = append(assembled, part...)
		packets++
	}
	var values []string
	if json.Unmarshal(assembled, &values) != nil || !reflect.DeepEqual(values, []string{value, value}) {
		t.Fatal("worst-case payload changed")
	}
	// Include start and EOF. The first of these notifications needs no delay.
	pacedDuration := time.Duration(packets+1) * extRespMinInterval
	if len(assembled) != 3079 || packets != 1324 || pacedDuration != 132500*time.Millisecond {
		t.Fatalf("bytes=%d packets=%d pacing=%v", len(assembled), packets, pacedDuration)
	}
	if pacedDuration >= hashSnapshotTTL {
		t.Fatal("snapshot expires before a compliant worst-case transfer")
	}
	t.Logf("MTU23 maximum escaped values: %d bytes, %d data packets, %v minimum pacing; TTL %v", len(assembled), packets, pacedDuration, hashSnapshotTTL)
}
