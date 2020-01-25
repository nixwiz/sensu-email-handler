package main

import (
	"bytes"
	"crypto/tls"
	"errors"
	"fmt"
	"io/ioutil"
	"math"
	"net/http"
	"net/mail"
	"net/smtp"
	"net/url"
	"strings"
	"text/template"

	corev2 "github.com/sensu/sensu-go/api/core/v2"
	"github.com/sensu/sensu-plugins-go-library/sensu"
)

type HandlerConfig struct {
	sensu.PluginConfig
	SmtpHost         string
	SmtpUsername     string
	SmtpPassword     string
	SmtpPort         uint64
	ToEmail          string
	FromEmail        string
	FromHeader       string
	AuthMethod       string
	TLSSkipVerify    bool
	Hookout          bool
	BodyTemplateFile string
	SubjectTemplate  string

	// deprecated options
	Insecure  bool
	LoginAuth bool
}

type loginAuth struct {
	username, password string
}

const (
	smtpHost         = "smtpHost"
	smtpUsername     = "smtpUsername"
	smtpPassword     = "smtpPassword"
	smtpPort         = "smtpPort"
	toEmail          = "toEmail"
	fromEmail        = "fromEmail"
	authMethod       = "authMethod"
	tlsSkipVerify    = "tlsSkipVerify"
	hookout          = "hookout"
	bodyTemplateFile = "bodyTemplateFile"
	subjectTemplate  = "subjectTemplate"
	defaultSmtpPort  = 587

	// deprecated options
	insecure        = "insecure"
	enableLoginAuth = "enableLoginAuth"
)

const (
	AuthMethodNone  = "none"
	AuthMethodPlain = "plain"
	AuthMethodLogin = "login"
)

var (
	config = HandlerConfig{
		PluginConfig: sensu.PluginConfig{
			Name:     "sensu-email-handler",
			Short:    "The Sensu Go Email handler for sending an email notification",
			Keyspace: "sensu.io/plugins/email/config",
		},
	}

	emailBodyTemplate = "{{.Check.Output}}"

	emailConfigOptions = []*sensu.PluginConfigOption{
		{
			Path:      smtpHost,
			Argument:  smtpHost,
			Shorthand: "s",
			Default:   "",
			Usage:     "The SMTP host to use to send to send email",
			Value:     &config.SmtpHost,
		},
		{
			Path:      smtpUsername,
			Env:       "SMTP_USERNAME",
			Argument:  smtpUsername,
			Shorthand: "u",
			Default:   "",
			Usage:     "The SMTP username, if not in env SMTP_USERNAME",
			Value:     &config.SmtpUsername,
		},
		{
			Path:      smtpPassword,
			Env:       "SMTP_PASSWORD",
			Argument:  smtpPassword,
			Shorthand: "p",
			Default:   "",
			Usage:     "The SMTP password, if not in env SMTP_PASSWORD",
			Value:     &config.SmtpPassword,
		},
		{
			Path:      smtpPort,
			Argument:  smtpPort,
			Shorthand: "P",
			Default:   uint64(defaultSmtpPort),
			Usage:     "The SMTP server port",
			Value:     &config.SmtpPort,
		},
		{
			Path:      toEmail,
			Argument:  toEmail,
			Shorthand: "t",
			Default:   "",
			Usage:     "The 'to' email address",
			Value:     &config.ToEmail,
		},
		{
			Path:      fromEmail,
			Argument:  fromEmail,
			Shorthand: "f",
			Default:   "",
			Usage:     "The 'from' email address",
			Value:     &config.FromEmail,
		},
		{
			Path:      tlsSkipVerify,
			Argument:  tlsSkipVerify,
			Shorthand: "k",
			Default:   false,
			Usage:     "Do not verify TLS certificates",
			Value:     &config.TLSSkipVerify,
		},
		{
			Path:      authMethod,
			Argument:  authMethod,
			Shorthand: "a",
			Default:   AuthMethodPlain,
			Usage:     "The SMTP authentication method, one of 'none', 'plain', or 'login'",
			Value:     &config.AuthMethod,
		},
		{
			Path:      hookout,
			Argument:  hookout,
			Shorthand: "H",
			Default:   false,
			Usage:     "Include output from check hook(s)",
			Value:     &config.Hookout,
		},
		{
			Path:      bodyTemplateFile,
			Argument:  bodyTemplateFile,
			Shorthand: "T",
			Default:   "",
			Usage:     "A template file to use for the body, specified  as fully qualified path or URL (file://, http://, https://)",
			Value:     &config.BodyTemplateFile,
		},
		{
			Path:      subjectTemplate,
			Argument:  subjectTemplate,
			Shorthand: "S",
			Default:   "Sensu Alert - {{.Entity.Name}}/{{.Check.Name}}: {{.Check.State}}",
			Usage:     "A template to use for the subject",
			Value:     &config.SubjectTemplate,
		},

		// deprecated options
		{
			Path:      insecure,
			Argument:  insecure,
			Shorthand: "i",
			Default:   false,
			Usage:     "[deprecated] Use an insecure connection (unauthenticated on port 25)",
			Value:     &config.Insecure,
		},
		{
			Path:      enableLoginAuth,
			Argument:  enableLoginAuth,
			Shorthand: "l",
			Default:   false,
			Usage:     "[deprecated] Use \"login auth\" mechanisim",
			Value:     &config.LoginAuth,
		},
	}
)

func main() {
	goHandler := sensu.NewGoHandler(&config.PluginConfig, emailConfigOptions, checkArgs, sendEmail)
	goHandler.Execute()
}

func checkArgs(_ *corev2.Event) error {
	if len(config.SmtpHost) == 0 {
		return errors.New("missing smtp host")
	}
	if config.SmtpPort > math.MaxUint16 {
		return errors.New("smtp port is out of range")
	}
	if len(config.ToEmail) == 0 {
		return errors.New("missing destination email address")
	}
	if len(config.FromEmail) == 0 {
		return errors.New("from email is empty")
	}

	// translate deprecated options to replacements
	if config.LoginAuth {
		config.AuthMethod = AuthMethodLogin
	}
	if config.Insecure {
		config.SmtpPort = 25
		config.AuthMethod = AuthMethodNone
		config.TLSSkipVerify = true
	}

	switch config.AuthMethod {
	case AuthMethodPlain, AuthMethodNone, AuthMethodLogin:
	case "":
		config.AuthMethod = AuthMethodPlain
	default:
		return fmt.Errorf("%s is not a valid auth method", config.AuthMethod)
	}
	if config.AuthMethod != AuthMethodNone {
		if len(config.SmtpUsername) == 0 {
			return errors.New("smtp username is empty")
		}
		if len(config.SmtpPassword) == 0 {
			return errors.New("smtp password is empty")
		}
	}

	if config.Hookout && len(config.BodyTemplateFile) > 0 {
		return errors.New("--hookout (-H) and --bodyTemplateFile (-T) are mutually exclusive")
	}
	if config.Hookout {
		emailBodyTemplate = "{{.Check.Output}}\n{{range .Check.Hooks}}Hook Name:  {{.Name}}\nHook Command:  {{.Command}}\n\n{{.Output}}\n\n{{end}}"
	} else if len(config.BodyTemplateFile) > 0 {
		templateBytes, fileErr := loadTemplateFile(config.BodyTemplateFile)
		if fileErr != nil {
			return fmt.Errorf("failed to read specified template file %s, %v", config.BodyTemplateFile, fileErr)
		}
		emailBodyTemplate = string(templateBytes)
	}

	fromAddr, addrErr := mail.ParseAddress(config.FromEmail)
	if addrErr != nil {
		return addrErr
	}
	config.FromEmail = fromAddr.Address
	config.FromHeader = fromAddr.String()
	return nil
}

func sendEmail(event *corev2.Event) error {
	var contentType string

	smtpAddress := fmt.Sprintf("%s:%d", config.SmtpHost, config.SmtpPort)
	subject, subjectErr := resolveTemplate(config.SubjectTemplate, event)
	if subjectErr != nil {
		return subjectErr
	}
	body, bodyErr := resolveTemplate(emailBodyTemplate, event)
	if bodyErr != nil {
		return bodyErr
	}

	if strings.Contains(body, "<html>") {
		contentType = "text/html"
	} else {
		contentType = "text/plain"
	}

	msg := []byte("From: " + config.FromHeader + "\r\n" +
		"To: " + config.ToEmail + "\r\n" +
		"Subject: " + subject + "\r\n" +
		"Content-Type: " + contentType + "\r\n" +
		"\r\n" +
		body + "\r\n")

	var auth smtp.Auth
	switch config.AuthMethod {
	case AuthMethodPlain:
		auth = smtp.PlainAuth("", config.SmtpUsername, config.SmtpPassword, config.SmtpHost)
	case AuthMethodLogin:
		auth = LoginAuth(config.SmtpUsername, config.SmtpPassword)
	}

	conn, err := smtp.Dial(smtpAddress)
	if err != nil {
		return err
	}
	defer conn.Close()

	if ok, _ := conn.Extension("STARTTLS"); ok {
		tlsConfig := &tls.Config{
			ServerName:         config.SmtpHost,
			InsecureSkipVerify: config.TLSSkipVerify,
		}
		if err := conn.StartTLS(tlsConfig); err != nil {
			return err
		}
	}

	if ok, _ := conn.Extension("AUTH"); ok && auth != nil {
		if err := conn.Auth(auth); err != nil {
			return err
		}
	}

	if err := conn.Mail(config.FromEmail); err != nil {
		return err
	}
	if err := conn.Rcpt(config.ToEmail); err != nil {
		return err
	}

	data, err := conn.Data()
	if err != nil {
		return err
	}
	if _, err := data.Write(msg); err != nil {
		return err
	}
	if err := data.Close(); err != nil {
		return err
	}

	return conn.Quit()
}

func resolveTemplate(templateValue string, event *corev2.Event) (string, error) {
	var resolved bytes.Buffer
	tmpl, err := template.New("test").Parse(templateValue)
	if err != nil {
		panic(err)
	}
	err = tmpl.Execute(&resolved, *event)
	if err != nil {
		panic(err)
	}

	return resolved.String(), nil
}

// https://gist.github.com/homme/22b457eb054a07e7b2fb
// https://gist.github.com/andelf/5118732

// MIT license (c) andelf 2013

func LoginAuth(username, password string) smtp.Auth {
	return &loginAuth{username, password}
}

func (a *loginAuth) Start(server *smtp.ServerInfo) (string, []byte, error) {
	return "LOGIN", []byte(a.username), nil
}

func (a *loginAuth) Next(fromServer []byte, more bool) ([]byte, error) {
	if more {
		switch string(fromServer) {
		case "Username:":
			return []byte(a.username), nil
		case "Password:":
			return []byte(a.password), nil
		default:
			return nil, fmt.Errorf("Unknown response (%s) from server when attempting to use loginAuth", string(fromServer))
		}
	}
	return nil, nil
}

func loadTemplateFile(templateFile string) ([]byte, error) {

	// Fix for relative paths on local file system? Sanity says it should
	// only ever be fully qualifed paths
	if strings.HasPrefix(templateFile, "/") {
		templateBytes, _ := ioutil.ReadFile(templateFile)
		return templateBytes, nil
	} else if strings.Contains(templateFile, "://") {
		u, err := url.Parse(templateFile)
		if err != nil {
			return nil, err
		}

		if strings.EqualFold(u.Scheme, "file") {
			templateBytes, _ := ioutil.ReadFile(u.Path)
			return templateBytes, nil
		} else if strings.EqualFold(u.Scheme, "http") || strings.EqualFold(u.Scheme, "https") {
			resp, err := http.Get(templateFile)
			if err != nil {
				return nil, err
			}
			defer resp.Body.Close()
			templateBytes, err := ioutil.ReadAll(resp.Body)
			if err != nil {
				return nil, err
			}
			return templateBytes, nil
		} else {
			return nil, fmt.Errorf("Unsupported scheme %v://\n", u.Scheme)
		}
	} else {
		return nil, fmt.Errorf("Not a fully qualified local file or URL: %s\n", templateFile)
	}
}
