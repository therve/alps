package koushinbase

import (
	"html/template"
	"net/url"
	"strings"
	"time"

	"github.com/emersion/go-imap"
)

var templateFuncs = template.FuncMap{
	"tuple": func(values ...interface{}) []interface{} {
		return values
	},
	"pathescape": func(s string) string {
		return url.PathEscape(s)
	},
	"formataddrlist": func(addrs []*imap.Address) string {
		l := make([]string, len(addrs))
		for i, addr := range addrs {
			l[i] = addr.PersonalName + " <" + addr.Address() + ">"
		}
		return strings.Join(l, ", ")
	},
	"formatdate": func(t time.Time) string {
		return t.Format("Mon Jan 02 15:04")
	},
	"formatflag": func(flag string) string {
		switch flag {
		case imap.SeenFlag:
			return "Seen"
		case imap.AnsweredFlag:
			return "Answered"
		case imap.FlaggedFlag:
			return "Starred"
		case imap.DraftFlag:
			return "Draft"
		default:
			return flag
		}
	},
	"ismutableflag": func(flag string) bool {
		switch flag {
		case imap.AnsweredFlag, imap.DeletedFlag, imap.DraftFlag:
			return false
		default:
			return true
		}
	},
}
