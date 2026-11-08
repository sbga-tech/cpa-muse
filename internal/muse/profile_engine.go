package muse

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"hash"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/tidwall/sjson"
	"golang.org/x/net/http/httpguts"
)

type profileInput struct {
	Body          []byte
	Headers       http.Header
	CustomHeaders http.Header
	CallerKey     string
	CredentialID  string
	RequestID     string
	State         *profileRequestState
}

// profileRequestState keeps active HTTP retry plans alive independently of global cache eviction.
// A zero value is ready for use; it must not be copied after first use or shared across HTTP requests.
// Its lifetime and distinct attempts are controlled by the owning request and existing manager retry limits.
type profileRequestState struct {
	mu         sync.Mutex
	templates  map[[32]byte]*planTemplate
	selections map[[32]byte]ProfileSelection
	anonymous  string
	wireMu     sync.Mutex
	wirePlans  map[[32]byte]*profilePlan
}

type profilePlan struct {
	body            []byte
	headers         http.Header
	transformations []string
	ephemeral       bool
}

func (p *profilePlan) Body() []byte              { return bytes.Clone(p.body) }
func (p *profilePlan) Headers() http.Header      { return p.headers.Clone() }
func (p *profilePlan) Transformations() []string { return append([]string(nil), p.transformations...) }
func (p *profilePlan) EphemeralSession() bool    { return p.ephemeral }

type profileSession struct {
	id    string
	trace string
}

// planTemplate retains generated identity and insertions, never the caller's prompt or headers.
type planTemplate struct {
	headers         http.Header
	insertions      []insertion
	transformations []string
	ephemeral       bool
}

type profileEngine struct {
	mu       sync.Mutex
	sessions *boundedCache[profileSession]
	requests *boundedCache[*planTemplate]
	now      func() time.Time
	entropy  io.Reader
}

// newProfileEngine bounds conversation state to 4096 entries/24 idle hours and retries to 512 entries/10 idle minutes.
func newProfileEngine() *profileEngine {
	return &profileEngine{
		sessions: newCache[profileSession](4096, 24*time.Hour),
		requests: newCache[*planTemplate](512, 10*time.Minute),
		now:      time.Now,
		entropy:  rand.Reader,
	}
}

func (e *profileEngine) Prepare(selection ProfileSelection, input profileInput) (*profilePlan, error) {
	if err := ValidateCustomHeaders(input.CustomHeaders); err != nil {
		return nil, err
	}
	if !selection.Enabled {
		return &profilePlan{body: bytes.Clone(input.Body), headers: input.CustomHeaders.Clone()}, nil
	}
	if e == nil || e.sessions == nil || e.requests == nil || e.now == nil || e.entropy == nil {
		return nil, invalidProfile()
	}
	if selection.Profile = strings.TrimSpace(selection.Profile); selection.Profile == "" {
		selection.Profile = defaultMuseProfile
	}
	object, err := decodeObject(input.Body)
	if err != nil {
		return nil, err
	}
	conversation, err := conversationIdentity(input.Headers, object)
	if err != nil {
		return nil, err
	}
	requestKey := requestIdentity(selection, input, conversation)
	if input.State != nil {
		input.State.mu.Lock()
		defer input.State.mu.Unlock()
		if cached := input.State.templates[requestKey]; cached != nil {
			return materialize(cached, input)
		}
	} else if input.RequestID != "" {
		e.mu.Lock()
		cached, found := e.requests.get(requestKey, e.now())
		e.mu.Unlock()
		if found {
			return materialize(cached, input)
		}
	}
	selected, err := loadProfile(selection.Profile)
	if err != nil {
		return nil, err
	}
	headers := make(http.Header, len(selected.StaticHeaders)+3)
	var transformations []string
	for name, value := range selected.StaticHeaders {
		headers.Set(name, value)
		transformations = append(transformations, "header:"+strings.ToLower(name))
	}
	// Profile headers are declarative; model/account routing is not inferred here.
	sort.Strings(transformations)
	if err := validateProfileHeaders(input, headers); err != nil {
		return nil, err
	}
	var insertions []insertion
	if selection.FillDefaults {
		insertions = defaultInsertions(object, selected.Defaults, "")
		for _, insertion := range insertions {
			transformations = append(transformations, "default:"+insertion.path)
		}
	}
	e.mu.Lock()
	template, err := e.prepareLocked(requestKey, input, conversation, object, headers, insertions, transformations)
	e.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if input.State != nil {
		if input.State.templates == nil {
			input.State.templates = make(map[[32]byte]*planTemplate)
		}
		input.State.templates[requestKey] = template
	}
	return materialize(template, input)
}

func (e *profileEngine) prepareLocked(requestKey [32]byte, input profileInput, conversation string, object map[string]json.RawMessage, headers http.Header, insertions []insertion, transformations []string) (*planTemplate, error) {
	now := e.now()
	if input.State == nil && input.RequestID != "" {
		if cached, found := e.requests.get(requestKey, now); found {
			return cached, nil
		}
	}
	ephemeral := conversation == ""
	sessionKey := digestStrings(input.CallerKey, conversation, input.CredentialID)
	current, found := e.sessions.get(sessionKey, now)
	if ephemeral || !found {
		id, err := uuid.NewV7FromReader(e.entropy)
		if err != nil {
			return nil, entropyFailure()
		}
		trace, err := randomHex(e.entropy, 16)
		if err != nil {
			return nil, err
		}
		current = profileSession{id: id.String(), trace: trace}
	}
	span, err := randomHex(e.entropy, 8)
	if err != nil {
		return nil, err
	}
	headers.Set("X-Tbh-Session-Id", current.id)
	headers.Set("X-Meta-Ai-Gateway-Session-Id", current.id)
	headers.Set("X-Tbh-Thread-Id", current.id)
	headers.Set("X-Tbh-Root-Session-Id", current.id)
	headers.Set("Traceparent", "00-"+current.trace+"-"+span+"-01")
	transformations = append(transformations, "header:x-tbh-session-id", "header:x-meta-ai-gateway-session-id", "header:x-tbh-thread-id", "header:x-tbh-root-session-id", "header:traceparent")
	if _, present := object["prompt_cache_key"]; !present {
		value, _ := json.Marshal("tbh:main:" + current.id)
		insertions = append(insertions, insertion{path: "prompt_cache_key", value: value})
		transformations = append(transformations, "prompt_cache_key")
	}
	template := &planTemplate{headers: headers, insertions: insertions, transformations: transformations, ephemeral: ephemeral}
	if !ephemeral {
		e.sessions.put(sessionKey, current, now)
	}
	if input.State == nil && input.RequestID != "" {
		e.requests.put(requestKey, template, now)
	}
	return template, nil
}

func materialize(template *planTemplate, input profileInput) (*profilePlan, error) {
	if err := validateProfileHeaders(input, template.headers); err != nil {
		return nil, err
	}
	body := bytes.Clone(input.Body)
	for _, insertion := range template.insertions {
		var err error
		body, err = sjson.SetRawBytes(body, insertion.path, insertion.value)
		if err != nil {
			return nil, invalidBody()
		}
	}
	headers := template.headers.Clone()
	for name, values := range input.CustomHeaders {
		headers[http.CanonicalHeaderKey(name)] = append([]string(nil), values...)
	}
	return &profilePlan{body: body, headers: headers, transformations: append([]string(nil), template.transformations...), ephemeral: template.ephemeral}, nil
}

// sameHeaderValues reports whether two header value lists are equal ignoring order and spacing.
func sameHeaderValues(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if strings.TrimSpace(a[i]) != strings.TrimSpace(b[i]) {
			return false
		}
	}
	return true
}

func validateProfileHeaders(input profileInput, expected http.Header) error {
	for name, values := range input.CustomHeaders {
		if !httpguts.ValidHeaderFieldName(name) || strings.EqualFold(name, "Host") {
			return identityConflict()
		}
		if want, reserved := expected[http.CanonicalHeaderKey(name)]; reserved {
			// A custom header may repeat a profile header as long as it says the same thing;
			// credentials commonly carry their own copy of X-Client-Id and friends, and
			// rejecting an identical value would make those credentials unusable with profiles.
			if !sameHeaderValues(values, want) {
				return identityConflict()
			}
		}
		for _, value := range values {
			if !httpguts.ValidHeaderFieldValue(value) {
				return identityConflict()
			}
		}
	}
	for _, name := range []string{"X-Client-Id", "X-Tbh-Agent-Role", "X-Tbh-Contributor-Mode"} {
		value, err := singleHeader(input.Headers, name)
		if err != nil || value != "" && value != expected.Get(name) {
			return identityConflict()
		}
	}
	return nil
}

func conversationIdentity(headers http.Header, body map[string]json.RawMessage) (string, error) {
	sessionID, err := singleHeader(headers, "X-Tbh-Session-Id")
	if err != nil {
		return "", err
	}
	gatewayID, err := singleHeader(headers, "X-Meta-Ai-Gateway-Session-Id")
	if err != nil || sessionID != "" && gatewayID != "" && sessionID != gatewayID {
		return "", identityConflict()
	}
	if sessionID != "" {
		return "header:x-tbh-session-id:" + sessionID, nil
	}
	var cacheKey string
	if json.Unmarshal(body["prompt_cache_key"], &cacheKey) == nil && strings.TrimSpace(cacheKey) != "" {
		return "body:prompt_cache_key:" + cacheKey, nil
	}
	for _, name := range []string{"Session_id", "Session-Id", "X-Codex-Session-Id", "X-Conversation-Id", "Conversation_id", "Conversation-Id", "X-Session-Id"} {
		value, err := singleHeader(headers, name)
		if err != nil {
			return "", err
		}
		if value != "" {
			return "header:" + strings.ToLower(name) + ":" + value, nil
		}
	}
	return "", nil
}

func singleHeader(headers http.Header, target string) (string, error) {
	var selected string
	for name, values := range headers {
		if !strings.EqualFold(name, target) {
			continue
		}
		for _, value := range values {
			value = strings.TrimSpace(value)
			if value == "" {
				continue
			}
			if strings.ContainsAny(value, "\r\n") || selected != "" && selected != value {
				return "", identityConflict()
			}
			selected = value
		}
	}
	return selected, nil
}

func requestIdentity(selection ProfileSelection, input profileInput, conversation string) [32]byte {
	bodyHash := sha256.Sum256(input.Body)
	hasher := sha256.New()
	for _, value := range []string{input.RequestID, input.CallerKey, input.CredentialID, selection.Profile, conversation} {
		writeDigestPart(hasher, value)
	}
	if selection.FillDefaults {
		_, _ = hasher.Write([]byte{1})
	} else {
		_, _ = hasher.Write([]byte{0})
	}
	_, _ = hasher.Write(bodyHash[:])
	writeHeaderDigest(hasher, input.Headers)
	writeHeaderDigest(hasher, input.CustomHeaders)
	return [32]byte(hasher.Sum(nil))
}

func writeHeaderDigest(hasher hash.Hash, headers http.Header) {
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	sort.Strings(names)
	writeDigestCount(hasher, len(names))
	for _, name := range names {
		writeDigestPart(hasher, name)
		writeDigestCount(hasher, len(headers[name]))
		for _, value := range headers[name] {
			writeDigestPart(hasher, value)
		}
	}
}

func digestStrings(values ...string) [32]byte {
	hasher := sha256.New()
	for _, value := range values {
		writeDigestPart(hasher, value)
	}
	return [32]byte(hasher.Sum(nil))
}

func writeDigestPart(hasher hash.Hash, value string) {
	writeDigestCount(hasher, len(value))
	_, _ = hasher.Write([]byte(value))
}

func writeDigestCount(hasher hash.Hash, count int) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(count))
	_, _ = hasher.Write(length[:])
}

func randomHex(reader io.Reader, count int) (string, error) {
	data := make([]byte, count)
	zero := make([]byte, count)
	for attempt := 0; attempt < 3; attempt++ {
		if _, err := io.ReadFull(reader, data); err != nil {
			return "", entropyFailure()
		}
		if !bytes.Equal(data, zero) {
			return hex.EncodeToString(data), nil
		}
	}
	return "", entropyFailure()
}

func entropyFailure() error {
	return &profileError{500, "identity_unavailable", "A profile session or trace identifier could not be generated"}
}
