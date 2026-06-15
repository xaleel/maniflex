package events

import "context"

// Mailer sends a transactional email. Implement using any provider
// (SMTP, SendGrid, Resend, Postmark, etc.).
type Mailer interface {
	Send(ctx context.Context, msg EmailMessage) error
}

// EmailMessage is the message passed to Mailer.Send.
type EmailMessage struct {
	To      string
	Subject string
	HTML    string
	Text    string
}

// EmailTemplateFunc builds an EmailMessage from an event.
// Return nil to skip sending (e.g. when template conditions are not met).
type EmailTemplateFunc func(ctx context.Context, e Event) *EmailMessage

// SendEmail returns a Handler that sends a transactional email for each event.
// Use it as a subscriber on a Bus:
//
//	bus.Subscribe(ctx, events.Subscription{
//	    Patterns: []string{"user.created"},
//	    Handler: events.SendEmail(mailer, func(ctx context.Context, e events.Event) *events.EmailMessage {
//	        var record map[string]any
//	        _ = json.Unmarshal(e.Data, &record)
//	        email, _ := record["email"].(string)
//	        return &events.EmailMessage{
//	            To:      email,
//	            Subject: "Welcome!",
//	            HTML:    "<p>Thanks for signing up.</p>",
//	        }
//	    }),
//	})
func SendEmail(mailer Mailer, tmpl EmailTemplateFunc) Handler {
	return func(ctx context.Context, e Event) error {
		msg := tmpl(ctx, e)
		if msg == nil {
			return nil
		}
		return mailer.Send(ctx, *msg)
	}
}
