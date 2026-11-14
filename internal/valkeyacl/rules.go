package valkeyacl

import (
	"fmt"
	"regexp"
	"strings"
)

// prefixPattern is what a tenant's key prefix may be: a DNS label, which a
// namespace is, so it holds no glob character and no "%".
var prefixPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// Tenant is what one tenant's user may do: the commands of a profile on the
// keys under "<Prefix>:", and the channels under it when Channels is set.
type Tenant struct {
	// Prefix is the tenant's namespace. The key pattern is "<Prefix>:*" and
	// nothing else: no field of a claim reaches it.
	Prefix string
	// Write adds the write commands (@write) to the read commands (@read).
	Write bool
	// Channels grants publish and subscribe on "<Prefix>:*" channels.
	Channels bool
}

// commandRules are the command rules of the tenant, in the order the server
// applies them: everything off, the connection commands a client needs to log
// in (and no more than the categories below leave), the read commands and,
// with Write, the write commands, then the categories that are never granted.
// The last rules also take out what the categories hold that reaches beyond the
// prefix: SCAN and RANDOMKEY list key names without checking the key
// patterns, and PUBSUB lists channel names.
func (t Tenant) commandRules() []string {
	rules := []string{"-@all", "+@connection", "+@read"}
	if t.Write {
		rules = append(rules, "+@write")
	}
	if t.Channels {
		rules = append(rules, "+@pubsub")
	}
	rules = append(rules, "-@dangerous", "-@admin", "-scan", "-randomkey")
	if t.Channels {
		rules = append(rules, "-pubsub")
	}
	return rules
}

// Commands is the user's command rules as ACL GETUSER shows them, which is the
// text of the rules in order.
func (t Tenant) Commands() string { return strings.Join(t.commandRules(), " ") }

// Keys is the user's key pattern as ACL GETUSER shows it. A read-only user
// has read access to the keys only.
func (t Tenant) Keys() string {
	if t.Write {
		return "~" + t.Prefix + ":*"
	}
	return "%R~" + t.Prefix + ":*"
}

// ChannelPattern is the user's channel pattern as ACL GETUSER shows it, empty for
// none.
func (t Tenant) ChannelPattern() string {
	if t.Channels {
		return "&" + t.Prefix + ":*"
	}
	return ""
}

// Rules are the ACL rules that make the user exactly this, whatever it was
// before (the keys, channels, selectors and commands are reset, the passwords
// are not): on, the key and channel patterns and the command rules. They hold
// no password. The prefix is checked again here: the CRD and the webhook have
// checked the namespace, and a pattern with a glob in it would reach beyond the
// prefix.
func (t Tenant) Rules() ([]string, error) {
	if !prefixPattern.MatchString(t.Prefix) {
		return nil, fmt.Errorf("%w: %q is not a valid key prefix", ErrInvalid, t.Prefix)
	}
	rules := []string{"on", "clearselectors", "resetkeys", t.Keys(), "resetchannels"}
	if t.Channels {
		rules = append(rules, t.ChannelPattern())
	}
	return append(rules, t.commandRules()...), nil
}

// Matches reports whether a user is what the tenant's rules make: on, with a
// password, with the tenant's key and channel patterns and command rules.
func (t Tenant) Matches(u UserInfo) bool {
	return u.Has("on") && !u.Has("nopass") && u.Keys == t.Keys() &&
		u.Channels == t.ChannelPattern() && u.Commands == t.Commands()
}
