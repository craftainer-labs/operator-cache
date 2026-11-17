//go:build integration && moto

package awscloud

// The fake in front of moto: it answers the calls moto 5.2.5 does not implement
// and forwards every other call to moto unchanged.
//
//   - ElastiCache: DescribeUserGroups and ModifyUserGroup (moto has no user
//     groups at all) and ModifyUser (moto cannot change a user). A user's access
//     string, password count and status are kept here as an overlay over what moto
//     reports for the user it made, and DescribeUsers is patched with it.
//   - MemoryDB: DescribeACLs and UpdateACL (moto has no ACLs). UpdateUser, which
//     moto has, is forwarded.
//
// Both services settle in two steps, as the real ones do: a write leaves the user
// (or the group or ACL) "modifying", and the next read of it reports "modifying"
// once and then "active".
//
// It is a stand-in for what the controller needs to see, not a model of AWS: it
// does not check IAM, does not normalise an access string and does not limit a
// user's passwords. The README names what that leaves unchecked.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	motoAccount = "123456789012"
	ecNamespace = "http://elasticache.amazonaws.com/doc/2015-02-02/"
)

// ecUser is what the fake overrides of a user moto made.
type ecUser struct {
	access    *string
	passwords *int
	busy      bool
}

type fake struct {
	moto   string
	region string
	srv    *httptest.Server
	client *http.Client

	mu sync.Mutex
	// The ElastiCache user group.
	group      string
	groupUsers []string
	groupBusy  bool
	users      map[string]*ecUser
	// The MemoryDB ACL.
	acl      string
	aclUsers []string
	aclBusy  bool
	calls    []string
}

// newFake starts the fake on a private, non-loopback address of this host (pkg/egress
// refuses loopback in both modes), in front of moto at motoURL. Its user group and
// its ACL are named group and acl.
func newFake(t *testing.T, motoURL, region, group, acl string) *fake {
	t.Helper()
	f := &fake{
		moto: strings.TrimSuffix(motoURL, "/"), region: region, client: &http.Client{Timeout: 30 * time.Second},
		group: group, groupUsers: []string{"default"}, users: map[string]*ecUser{},
		acl: acl, aclUsers: []string{"default"},
	}
	l, err := net.Listen("tcp", net.JoinHostPort(privateIP(t), "0"))
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	f.srv = httptest.NewUnstartedServer(f)
	_ = f.srv.Listener.Close()
	f.srv.Listener = l
	f.srv.Start()
	t.Cleanup(f.srv.Close)
	t.Cleanup(f.client.CloseIdleConnections)
	return f
}

// URL is the address the SDK clients are pointed at.
func (f *fake) URL() string { return f.srv.URL }

// called counts the calls of an operation the fake has seen, answered or forwarded.
func (f *fake) called(op string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == op {
			n++
		}
	}
	return n
}

// privateIP is a private IPv4 address of this host that is not loopback.
func privateIP(t *testing.T) string {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatalf("interface addresses: %v", err)
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			if ip := n.IP.To4(); ip != nil && ip.IsPrivate() && !ip.IsLoopback() {
				return ip.String()
			}
		}
	}
	t.Fatal("this host has no private, non-loopback IPv4 address")
	return ""
}

func (f *fake) record(op string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, op)
}

// ServeHTTP implements http.Handler.
func (f *fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if op, ok := strings.CutPrefix(r.Header.Get("X-Amz-Target"), "AmazonMemoryDB."); ok {
		f.record(op)
		f.memorydb(w, r, op, body)
		return
	}
	form, err := url.ParseQuery(string(body))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f.record(form.Get("Action"))
	f.elasticache(w, r, form)
}

// do sends a request to moto with the headers of the SDK's (which carry the
// credential scope moto picks the service and region from).
func (f *fake) do(ctx context.Context, hdr http.Header, method, uri string, body []byte) (int, http.Header, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, f.moto+uri, bytes.NewReader(body)) //nolint:gosec // the test fake relays to the moto container this test started
	if err != nil {
		return 0, nil, nil, err
	}
	req.Header = hdr.Clone()
	req.Header.Del("Accept-Encoding") // the transport then decompresses for us
	req.Header.Del("Content-Length")
	resp, err := f.client.Do(req) //nolint:gosec // the test fake relays to the moto container this test started
	if err != nil {
		return 0, nil, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, out, err
}

// forward relays the request to moto and returns what moto answered.
func (f *fake) forward(r *http.Request, body []byte) (int, http.Header, []byte, error) {
	return f.do(r.Context(), r.Header, r.Method, r.URL.RequestURI(), body)
}

func relay(w http.ResponseWriter, status int, hdr http.Header, body []byte) {
	for k, v := range hdr {
		switch http.CanonicalHeaderKey(k) {
		case "Content-Length", "Content-Encoding", "Transfer-Encoding", "Connection":
		default:
			w.Header()[k] = v
		}
	}
	w.WriteHeader(status)
	_, _ = w.Write(body) //nolint:gosec // moto's own answer, relayed unchanged to the SDK of the test
}

func (f *fake) relayForward(w http.ResponseWriter, r *http.Request, body []byte) {
	status, hdr, out, err := f.forward(r, body)
	if err != nil {
		http.Error(w, "moto: "+err.Error(), http.StatusBadGateway)
		return
	}
	relay(w, status, hdr, out)
}

// ---- ElastiCache (query protocol, XML) ----

func ecResponse(action, result string) string {
	return `<` + action + `Response xmlns="` + ecNamespace + `"><` + action + `Result>` + result + `</` + action + `Result>` +
		`<ResponseMetadata><RequestId>fake</RequestId></ResponseMetadata></` + action + `Response>`
}

func ecError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "text/xml")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `<ErrorResponse xmlns="%s"><Error><Type>Sender</Type><Code>%s</Code><Message>%s</Message></Error><RequestId>fake</RequestId></ErrorResponse>`,
		ecNamespace, code, html.EscapeString(msg))
}

func ecOK(w http.ResponseWriter, xml string) {
	w.Header().Set("Content-Type", "text/xml")
	_, _ = io.WriteString(w, xml)
}

// groupXML is the user group's fields; its status is what the group reports now.
func (f *fake) groupXML(status string) string {
	var ids strings.Builder
	for _, u := range f.groupUsers {
		ids.WriteString("<member>" + html.EscapeString(u) + "</member>")
	}
	return "<UserGroupId>" + html.EscapeString(f.group) + "</UserGroupId><Status>" + status + "</Status><Engine>redis</Engine>" +
		"<UserIds>" + ids.String() + "</UserIds><ARN>arn:aws:elasticache:" + f.region + ":" + motoAccount + ":usergroup:" + html.EscapeString(f.group) + "</ARN>"
}

// readStatus is the status of the user group as a read reports it: "modifying"
// once after a write, then "active".
func (f *fake) readGroupStatus() string {
	if f.groupBusy {
		f.groupBusy = false
		return "modifying"
	}
	return "active"
}

func (f *fake) elasticache(w http.ResponseWriter, r *http.Request, form url.Values) {
	body := []byte(form.Encode())
	switch form.Get("Action") {
	case "DescribeUserGroups":
		f.mu.Lock()
		defer f.mu.Unlock()
		if id := form.Get("UserGroupId"); id != "" && id != f.group {
			ecError(w, http.StatusNotFound, "UserGroupNotFound", "user group "+id+" not found")
			return
		}
		ecOK(w, ecResponse("DescribeUserGroups", "<UserGroups><member>"+f.groupXML(f.readGroupStatus())+"</member></UserGroups>"))
	case "ModifyUserGroup":
		f.mu.Lock()
		defer f.mu.Unlock()
		if id := form.Get("UserGroupId"); id != f.group {
			ecError(w, http.StatusNotFound, "UserGroupNotFound", "user group "+id+" not found")
			return
		}
		for k, v := range form {
			if strings.HasPrefix(k, "UserIdsToAdd.") && len(v) > 0 && !slices.Contains(f.groupUsers, v[0]) {
				f.groupUsers = append(f.groupUsers, v[0])
			}
		}
		f.groupBusy = true
		ecOK(w, ecResponse("ModifyUserGroup", f.groupXML("modifying")))
	case "ModifyUser":
		id := form.Get("UserId")
		// The user must exist in moto: ask it, and relay its refusal.
		status, hdr, out, err := f.do(r.Context(), r.Header, http.MethodPost, "/",
			[]byte(url.Values{"Action": {"DescribeUsers"}, "Version": {"2015-02-02"}, "UserId": {id}}.Encode()))
		if err != nil {
			http.Error(w, "moto: "+err.Error(), http.StatusBadGateway)
			return
		}
		if status != http.StatusOK {
			relay(w, status, hdr, out)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		o := f.users[id]
		if o == nil {
			o = &ecUser{}
			f.users[id] = o
		}
		if v, ok := form["AccessString"]; ok {
			o.access = &v[0]
		}
		if form.Get("AuthenticationMode.Type") != "" {
			n := 0
			for k := range form {
				if strings.HasPrefix(k, "AuthenticationMode.Passwords.") {
					n++
				}
			}
			o.passwords = &n
		}
		o.busy = true
		ecOK(w, ecResponse("ModifyUser", "<UserId>"+html.EscapeString(id)+"</UserId><Status>modifying</Status>"))
	case "DescribeUsers":
		status, hdr, out, err := f.forward(r, body)
		if err != nil {
			http.Error(w, "moto: "+err.Error(), http.StatusBadGateway)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if o := f.users[form.Get("UserId")]; o != nil && status == http.StatusOK {
			out = []byte(o.patch(string(out)))
			o.busy = false
		}
		relay(w, status, hdr, out)
	case "DeleteUser":
		status, hdr, out, err := f.forward(r, body)
		if err != nil {
			http.Error(w, "moto: "+err.Error(), http.StatusBadGateway)
			return
		}
		if status == http.StatusOK {
			f.mu.Lock()
			f.groupUsers = slices.DeleteFunc(f.groupUsers, func(u string) bool { return u == form.Get("UserId") })
			delete(f.users, form.Get("UserId"))
			f.mu.Unlock()
		}
		relay(w, status, hdr, out)
	default:
		f.relayForward(w, r, body)
	}
}

var (
	reStatus    = regexp.MustCompile(`<Status>[^<]*</Status>`)
	reAccess    = regexp.MustCompile(`<AccessString>[^<]*</AccessString>`)
	rePasswords = regexp.MustCompile(`<PasswordCount>[^<]*</PasswordCount>`)
)

// patch puts the overlay over moto's description of the user: the first status
// (the user's), the access string and the password count.
func (o *ecUser) patch(xml string) string {
	if o.busy {
		done := false
		xml = reStatus.ReplaceAllStringFunc(xml, func(s string) string {
			if done {
				return s
			}
			done = true
			return "<Status>modifying</Status>"
		})
	}
	if o.access != nil {
		xml = reAccess.ReplaceAllLiteralString(xml, "<AccessString>"+html.EscapeString(*o.access)+"</AccessString>")
	}
	if o.passwords != nil {
		xml = rePasswords.ReplaceAllLiteralString(xml, "<PasswordCount>"+strconv.Itoa(*o.passwords)+"</PasswordCount>")
	}
	return xml
}

// ---- MemoryDB (AWS JSON 1.1) ----

func mdbError(w http.ResponseWriter, status int, typ, msg string) {
	w.Header().Set("Content-Type", "application/x-amz-json-1.1")
	w.Header().Set("X-Amzn-Errortype", typ)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"__type": typ, "message": msg})
}

func (f *fake) aclJSON(status string) map[string]any {
	return map[string]any{
		"Name": f.acl, "Status": status, "UserNames": f.aclUsers,
		"ARN": "arn:aws:memorydb:" + f.region + ":" + motoAccount + ":acl/" + f.acl,
	}
}

func (f *fake) memorydb(w http.ResponseWriter, r *http.Request, op string, body []byte) {
	var in struct {
		ACLName        string
		UserNamesToAdd []string
		UserName       string
	}
	if err := json.Unmarshal(body, &in); err != nil {
		mdbError(w, http.StatusBadRequest, "SerializationException", err.Error())
		return
	}
	reply := func(v any) {
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		_ = json.NewEncoder(w).Encode(v)
	}
	switch op {
	case "DescribeACLs":
		f.mu.Lock()
		defer f.mu.Unlock()
		if in.ACLName != "" && in.ACLName != f.acl {
			mdbError(w, http.StatusBadRequest, "ACLNotFoundFault", "ACL "+in.ACLName+" not found")
			return
		}
		status := "active"
		if f.aclBusy {
			f.aclBusy, status = false, "modifying"
		}
		reply(map[string]any{"ACLs": []any{f.aclJSON(status)}})
	case "UpdateACL":
		f.mu.Lock()
		defer f.mu.Unlock()
		if in.ACLName != f.acl {
			mdbError(w, http.StatusBadRequest, "ACLNotFoundFault", "ACL "+in.ACLName+" not found")
			return
		}
		for _, u := range in.UserNamesToAdd {
			if !slices.Contains(f.aclUsers, u) {
				f.aclUsers = append(f.aclUsers, u)
			}
		}
		f.aclBusy = true
		reply(map[string]any{"ACL": f.aclJSON("modifying")})
	case "DeleteUser":
		status, hdr, out, err := f.forward(r, body)
		if err != nil {
			http.Error(w, "moto: "+err.Error(), http.StatusBadGateway)
			return
		}
		if status == http.StatusOK {
			f.mu.Lock()
			f.aclUsers = slices.DeleteFunc(f.aclUsers, func(u string) bool { return u == in.UserName })
			f.mu.Unlock()
		}
		relay(w, status, hdr, out)
	default:
		f.relayForward(w, r, body)
	}
}
