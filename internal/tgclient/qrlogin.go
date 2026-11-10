package tgclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/gotd/td/telegram"
	"rsc.io/qr"
)

// qrPollInterval is how often LoginQR observes the flow for a new token, a
// 2FA prompt or the end of the login.
const qrPollInterval = 250 * time.Millisecond

// qrLoginFlow is the slice of *QRFlow that LoginQR drives, so the loop can be
// tested without a Telegram connection.
type qrLoginFlow interface {
	Observe() QRSnapshot
	Result() (QRResult, bool)
	SubmitPassword(pw string) bool
	Abort()
}

// LoginQR signs in by having an already logged-in Telegram app scan a QR code
// rendered in the terminal (Settings → Devices → Link Desktop Device). It
// needs no phone number and no login code, so it works when Telegram does not
// deliver the code. The session is stored where `run` loads it.
func LoginQR(ctx context.Context, cfg *Config, in io.Reader, out io.Writer) error {
	storage, err := NewSessionStorage()
	if err != nil {
		return fmt.Errorf("opening session storage: %w", err)
	}

	authorized := false
	client := newClient(ctx, cfg, storage, slog.Default(), telegram.Options{})
	err = client.Run(ctx, func(ctx context.Context) error {
		status, err := client.Auth().Status(ctx)
		if err != nil {
			return fmt.Errorf("checking auth status: %w", err)
		}
		if status.Authorized {
			authorized = true
			_, _ = fmt.Fprintf(out, "Already logged in as %s\n", UserName(status.User))
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("checking existing session: %w", err)
	}
	if authorized {
		return nil
	}

	flow, err := StartQRLogin(ctx, cfg)
	if err != nil {
		return fmt.Errorf("starting QR login: %w", err)
	}
	defer flow.Abort()

	result, err := driveQRLogin(ctx, flow, newUserAuthenticator("", in, out), out, qrPollInterval)
	if err != nil {
		return err
	}
	if err := storage.StoreSession(ctx, result.SessionData); err != nil {
		return fmt.Errorf("saving session: %w", err)
	}
	_, _ = fmt.Fprintf(out, "Successfully logged in as %s\n", qrUserName(result.User))
	_, _ = fmt.Fprintln(out, "You can now use the mcp-telegram server.")
	return nil
}

// driveQRLogin shows every new QR token, asks for the 2FA password when the
// account has one, and returns the final session once the flow is done.
func driveQRLogin(ctx context.Context, flow qrLoginFlow, prompt userAuthenticator, out io.Writer, interval time.Duration) (QRResult, error) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	shownRevision := -1
	reportedRejections := 0
	for {
		s := flow.Observe()
		switch s.State {
		case QRWaiting:
			if s.TokenURL != "" && s.QRRevision != shownRevision {
				if err := renderQR(out, s.TokenURL); err != nil {
					return QRResult{}, err
				}
				_, _ = fmt.Fprintln(out, "Scan this code in Telegram: Settings → Devices → Link Desktop Device.")
				_, _ = fmt.Fprintln(out, "It refreshes on its own until scanned.")
				shownRevision = s.QRRevision
			}
		case QRPasswordNeeded:
			if s.Rejected > reportedRejections {
				_, _ = fmt.Fprintf(out, "Wrong 2FA password, %d attempt(s) left.\n", s.AttemptsLeft)
				reportedRejections = s.Rejected
			}
			password, err := prompt.Password(ctx)
			if err != nil {
				return QRResult{}, err
			}
			flow.SubmitPassword(password)
		case QRPasswordVerifying:
			// Wait for Telegram to answer the password check.
		case QRDone:
			result, ok := flow.Result()
			if !ok {
				return QRResult{}, errors.New("QR login finished without a session")
			}
			return result, nil
		case QRFailed:
			return QRResult{}, fmt.Errorf("QR login failed: %w", s.Err)
		}

		select {
		case <-ctx.Done():
			return QRResult{}, fmt.Errorf("QR login interrupted: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func qrUserName(u QRUser) string {
	if u.Username != "" {
		return "@" + u.Username
	}
	return fmt.Sprintf("user %d", u.ID)
}

// renderQR writes url as a QR code drawn with Unicode half blocks, two module
// rows per text line, with the four-module quiet zone scanners expect. Each
// line pins white on black so the code reads the same on light and
// dark terminal themes: filled blocks are the light modules.
func renderQR(out io.Writer, url string) error {
	code, err := qr.Encode(url, qr.M)
	if err != nil {
		return fmt.Errorf("encoding QR code: %w", err)
	}
	const quiet = 4
	size := code.Size
	light := func(x, y int) bool {
		if x < 0 || y < 0 || x >= size || y >= size {
			return true
		}
		return !code.Black(x, y)
	}

	const whiteOnBlack, reset = "\x1b[97;40m", "\x1b[0m"
	var b []byte
	for y := -quiet; y < size+quiet; y += 2 {
		b = append(b, whiteOnBlack...)
		for x := -quiet; x < size+quiet; x++ {
			top, bottom := light(x, y), light(x, y+1)
			switch {
			case top && bottom:
				b = append(b, "█"...)
			case top:
				b = append(b, "▀"...)
			case bottom:
				b = append(b, "▄"...)
			default:
				b = append(b, ' ')
			}
		}
		b = append(b, reset...)
		b = append(b, '\n')
	}
	if _, err := out.Write(b); err != nil {
		return fmt.Errorf("writing QR code: %w", err)
	}
	return nil
}
