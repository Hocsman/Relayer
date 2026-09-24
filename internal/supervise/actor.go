package supervise

import (
	"strings"

	"github.com/Hocsman/Relayer/internal/audit"
)

// Actor is who asked the core for a decision or a line: the person, the role
// their front end gave them, and the connection they acted from.
//
// The desktop has one operator, at the machine, and passes the zero Actor: its
// journal names nobody, exactly as it always has. The web gateway serves
// several people at once, some of whom may only watch, and names each
// connection's signed-in identity, its role and its connection ID. The journal
// keeps them on the entries of a person's decision, of its delivery and of
// their lines; the connection is what ties an answer to the attach and control
// records around it, since one identity may be signed in from several tabs.
type Actor struct {
	Identity string
	Role     string
	ConnID   string
}

// The roles an Actor may have. They are the web gateway's own words, so that
// it passes a connection's role through unchanged.
const (
	// RoleOperator may answer prompts and send lines.
	RoleOperator = "operator"
	// RoleViewer may only watch: every operation that acts on an agent
	// refuses it with ErrReadOnlyActor.
	RoleViewer = "viewer"
)

// readOnly reports whether the actor may only watch. Every role but
// RoleOperator is read-only: RoleViewer, and any role the core does not know,
// so that a role a front end adds later acts on nothing until the core is
// taught what it may do. No role at all is the desktop's operator, which has
// no roles.
//
// The gateway already keeps viewers away from these operations twice, by the
// list of calls a viewer may make and by refusing it the terminal. The core
// is the third gate, and the one that holds whatever the front end forgot:
// the byte a viewer could otherwise send is an answer to an agent.
func (a Actor) readOnly() bool {
	role := strings.ToLower(strings.TrimSpace(a.Role))
	return role != "" && role != RoleOperator
}

// attributed is entry as the actor made it: the Operator, and the operator,
// role and connection in the metadata, each when the front end named it. The
// zero Actor names nobody, and the entry is the desktop's as it always was.
// It is for a person's decision and its delivery only; the policy's entries
// never name anybody, and a line's entry, whose shape is closed, names only
// its Operator (operatorInputAuditEntry).
func attributed(entry audit.Entry, actor Actor) audit.Entry {
	if actor == (Actor{}) {
		return entry
	}
	entry.Operator = strings.TrimSpace(actor.Identity)
	metadata := make(map[string]string, 3)
	for key, value := range map[string]string{
		"operator": actor.Identity,
		"role":     actor.Role,
		"conn_id":  actor.ConnID,
	} {
		if value = strings.TrimSpace(value); value != "" {
			metadata[key] = value
		}
	}
	if len(metadata) > 0 {
		entry.Metadata = metadata
	}
	return entry
}
