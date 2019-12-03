package koushin

import (
	"fmt"
	"io/ioutil"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	imapclient "github.com/emersion/go-imap/client"
	"github.com/emersion/go-sasl"
	"github.com/labstack/echo/v4"
)

const cookieName = "koushin_session"

type Server struct {
	imap struct {
		host     string
		tls      bool
		insecure bool

		pool *ConnPool
	}

	smtp struct {
		host     string
		tls      bool
		insecure bool
	}
}

func (s *Server) parseIMAPURL(imapURL string) error {
	u, err := url.Parse(imapURL)
	if err != nil {
		return fmt.Errorf("failed to parse IMAP server URL: %v", err)
	}

	s.imap.host = u.Host
	switch u.Scheme {
	case "imap":
		// This space is intentionally left blank
	case "imaps":
		s.imap.tls = true
	case "imap+insecure":
		s.imap.insecure = true
	default:
		return fmt.Errorf("unrecognized IMAP URL scheme: %s", u.Scheme)
	}

	return nil
}

func (s *Server) parseSMTPURL(smtpURL string) error {
	u, err := url.Parse(smtpURL)
	if err != nil {
		return fmt.Errorf("failed to parse SMTP server URL: %v", err)
	}

	s.smtp.host = u.Host
	switch u.Scheme {
	case "smtp":
		// This space is intentionally left blank
	case "smtps":
		s.smtp.tls = true
	case "smtp+insecure":
		s.smtp.insecure = true
	default:
		return fmt.Errorf("unrecognized SMTP URL scheme: %s", u.Scheme)
	}

	return nil
}

func NewServer(imapURL, smtpURL string) (*Server, error) {
	s := &Server{}

	if err := s.parseIMAPURL(imapURL); err != nil {
		return nil, err
	}
	s.imap.pool = NewConnPool()

	if smtpURL != "" {
		if err := s.parseSMTPURL(smtpURL); err != nil {
			return nil, err
		}
	}

	return s, nil
}

type context struct {
	echo.Context
	server  *Server
	session *Session
	conn    *imapclient.Client
}

var aLongTimeAgo = time.Unix(233431200, 0)

func (c *context) setToken(token string) {
	cookie := http.Cookie{
		Name:     cookieName,
		Value:    token,
		HttpOnly: true,
		// TODO: domain, secure
	}
	if token == "" {
		cookie.Expires = aLongTimeAgo // unset the cookie
	}
	c.SetCookie(&cookie)
}

func handleLogin(ectx echo.Context) error {
	ctx := ectx.(*context)
	username := ctx.FormValue("username")
	password := ctx.FormValue("password")
	if username != "" && password != "" {
		conn, err := ctx.server.connectIMAP()
		if err != nil {
			return err
		}

		if err := conn.Login(username, password); err != nil {
			conn.Logout()
			return ctx.Render(http.StatusOK, "login.html", nil)
		}

		token, err := ctx.server.imap.pool.Put(conn, username, password)
		if err != nil {
			return fmt.Errorf("failed to put connection in pool: %v", err)
		}
		ctx.setToken(token)

		return ctx.Redirect(http.StatusFound, "/mailbox/INBOX")
	}

	return ctx.Render(http.StatusOK, "login.html", nil)
}

func handleGetPart(ctx *context, raw bool) error {
	mboxName := ctx.Param("mbox")
	uid, err := parseUid(ctx.Param("uid"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err)
	}
	partPathString := ctx.QueryParam("part")
	partPath, err := parsePartPath(partPathString)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err)
	}

	msg, part, err := getMessagePart(ctx.conn, mboxName, uid, partPath)
	if err != nil {
		return err
	}

	mimeType, _, err := part.Header.ContentType()
	if err != nil {
		return fmt.Errorf("failed to parse part Content-Type: %v", err)
	}
	if len(partPath) == 0 {
		mimeType = "message/rfc822"
	}

	if raw {
		disp, dispParams, _ := part.Header.ContentDisposition()
		filename := dispParams["filename"]

		// TODO: set Content-Length if possible

		if !strings.EqualFold(mimeType, "text/plain") || strings.EqualFold(disp, "attachment") {
			dispParams := make(map[string]string)
			if filename != "" {
				dispParams["filename"] = filename
			}
			disp := mime.FormatMediaType("attachment", dispParams)
			ctx.Response().Header().Set("Content-Disposition", disp)
		}
		return ctx.Stream(http.StatusOK, mimeType, part.Body)
	}

	var body string
	if strings.HasPrefix(strings.ToLower(mimeType), "text/") {
		b, err := ioutil.ReadAll(part.Body)
		if err != nil {
			return fmt.Errorf("failed to read part body: %v", err)
		}
		body = string(b)
	}

	return ctx.Render(http.StatusOK, "message.html", map[string]interface{}{
		"Mailbox":  ctx.conn.Mailbox(),
		"Message":  msg,
		"Body":     body,
		"PartPath": partPathString,
	})
}

func handleCompose(ectx echo.Context) error {
	ctx := ectx.(*context)

	if ctx.Request().Method == http.MethodPost {
		// TODO: parse address lists
		from := ctx.FormValue("from")
		to := ctx.FormValue("to")
		subject := ctx.FormValue("subject")
		text := ctx.FormValue("text")

		c, err := ctx.server.connectSMTP()
		if err != nil {
			return err
		}
		defer c.Close()

		auth := sasl.NewPlainClient("", ctx.session.username, ctx.session.password)
		if err := c.Auth(auth); err != nil {
			return echo.NewHTTPError(http.StatusForbidden, err)
		}

		msg := OutgoingMessage{
			from: from,
			to: []string{to},
			subject: subject,
			text: text,
		}
		if err := sendMessage(c, &msg); err != nil {
			return err
		}

		if err := c.Quit(); err != nil {
			return fmt.Errorf("QUIT failed: %v", err)
		}

		// TODO: append to IMAP Sent mailbox

		return ctx.Redirect(http.StatusFound, "/mailbox/INBOX")
	}

	return ctx.Render(http.StatusOK, "compose.html", nil)
}

func New(imapURL, smtpURL string) *echo.Echo {
	e := echo.New()

	s, err := NewServer(imapURL, smtpURL)
	if err != nil {
		e.Logger.Fatal(err)
	}

	e.HTTPErrorHandler = func(err error, c echo.Context) {
		code := http.StatusInternalServerError
		if he, ok := err.(*echo.HTTPError); ok {
			code = he.Code
		} else {
			c.Logger().Error(err)
		}
		// TODO: hide internal errors
		c.String(code, err.Error())
	}

	e.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(ectx echo.Context) error {
			ctx := &context{Context: ectx, server: s}

			cookie, err := ctx.Cookie(cookieName)
			if err == http.ErrNoCookie {
				// Require auth for all pages except /login
				if ctx.Path() == "/login" {
					return next(ctx)
				} else {
					return ctx.Redirect(http.StatusFound, "/login")
				}
			} else if err != nil {
				return err
			}

			ctx.session, err = ctx.server.imap.pool.Get(cookie.Value)
			if err == ErrSessionExpired {
				ctx.setToken("")
				return ctx.Redirect(http.StatusFound, "/login")
			} else if err != nil {
				return err
			}
			ctx.conn = ctx.session.imapConn

			return next(ctx)
		}
	})

	e.Renderer, err = loadTemplates()
	if err != nil {
		e.Logger.Fatal("Failed to load templates:", err)
	}

	e.GET("/mailbox/:mbox", func(ectx echo.Context) error {
		ctx := ectx.(*context)

		mailboxes, err := listMailboxes(ctx.conn)
		if err != nil {
			return err
		}

		msgs, err := listMessages(ctx.conn, ctx.Param("mbox"))
		if err != nil {
			return err
		}

		return ctx.Render(http.StatusOK, "mailbox.html", map[string]interface{}{
			"Mailbox":   ctx.conn.Mailbox(),
			"Mailboxes": mailboxes,
			"Messages":  msgs,
		})
	})

	e.GET("/message/:mbox/:uid", func(ectx echo.Context) error {
		ctx := ectx.(*context)
		return handleGetPart(ctx, false)
	})
	e.GET("/message/:mbox/:uid/raw", func(ectx echo.Context) error {
		ctx := ectx.(*context)
		return handleGetPart(ctx, true)
	})

	e.GET("/login", handleLogin)
	e.POST("/login", handleLogin)

	e.GET("/logout", func(ectx echo.Context) error {
		ctx := ectx.(*context)
		if err := ctx.conn.Logout(); err != nil {
			return fmt.Errorf("failed to logout: %v", err)
		}
		ctx.setToken("")
		return ctx.Redirect(http.StatusFound, "/login")
	})

	e.GET("/compose", handleCompose)
	e.POST("/compose", handleCompose)

	e.Static("/assets", "public/assets")

	return e
}
