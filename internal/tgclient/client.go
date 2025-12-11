package tgclient

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/gotd/contrib/middleware/floodwait"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/tg"
	"golang.org/x/term"
)

// Config holds Telegram API credentials
type Config struct {
	APIID   int
	APIHash string
}

// userAuthenticator implements auth.UserAuthenticator
type userAuthenticator struct {
	phone string
}

func (a userAuthenticator) Phone(ctx context.Context) (string, error) {
	return a.phone, nil
}

func (a userAuthenticator) Code(ctx context.Context, sentCode *tg.AuthSentCode) (string, error) {
	fmt.Print("Enter login code: ")
	reader := bufio.NewReader(os.Stdin)
	code, err := reader.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("reading code: %w", err)
	}
	return strings.TrimSpace(code), nil
}

func (a userAuthenticator) Password(ctx context.Context) (string, error) {
	fmt.Print("Enter 2FA password: ")

	// Use hidden input if running in a real terminal, otherwise fall back to plain input
	if term.IsTerminal(int(os.Stdin.Fd())) {
		password, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println() // Print newline after hidden input
		if err != nil {
			return "", fmt.Errorf("reading password: %w", err)
		}
		return string(password), nil
	}

	// Fallback for non-TTY environments (e.g., IDE)
	reader := bufio.NewReader(os.Stdin)
	password, err := reader.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("reading password: %w", err)
	}
	return strings.TrimSpace(password), nil
}

func (a userAuthenticator) AcceptTermsOfService(ctx context.Context, tos tg.HelpTermsOfService) error {
	return nil
}

func (a userAuthenticator) SignUp(ctx context.Context) (auth.UserInfo, error) {
	return auth.UserInfo{}, fmt.Errorf("sign up is not supported")
}

// CreateClient creates a new Telegram client with session storage and flood wait handling.
// Returns the client and a floodwait.Waiter that should wrap the client.Run() call.
func CreateClient(cfg *Config) (*telegram.Client, *floodwait.Waiter) {
	storage := NewSessionStorage()
	waiter := floodwait.NewWaiter().WithMaxWait(60 * time.Second)

	client := telegram.NewClient(cfg.APIID, cfg.APIHash, telegram.Options{
		SessionStorage: storage,
		Middlewares:    []telegram.Middleware{waiter},
	})

	return client, waiter
}

// Login performs interactive sign-in to Telegram
func Login(ctx context.Context, cfg *Config, phone string) error {
	client, waiter := CreateClient(cfg)

	err := waiter.Run(ctx, func(ctx context.Context) error {
		return client.Run(ctx, func(ctx context.Context) error {
			// Check if already authorized
			status, err := client.Auth().Status(ctx)
			if err != nil {
				return fmt.Errorf("checking auth status: %w", err)
			}

			if status.Authorized {
				user, err := client.Self(ctx)
				if err == nil {
					fmt.Printf("Already logged in as @%s\n", user.Username)
				}
				return nil
			}

			// Perform authentication
			flow := auth.NewFlow(
				userAuthenticator{phone: phone},
				auth.SendCodeOptions{},
			)

			if err := flow.Run(ctx, client.Auth()); err != nil {
				return fmt.Errorf("running auth flow: %w", err)
			}

			user, err := client.Self(ctx)
			if err != nil {
				return fmt.Errorf("getting user info: %w", err)
			}

			fmt.Printf("Successfully logged in as @%s\n", user.Username)
			fmt.Println("You can now use the mcp-telegram server.")

			return nil
		})
	})
	if err != nil {
		return fmt.Errorf("logging in: %w", err)
	}
	return nil
}

// Logout logs out from Telegram
func Logout(ctx context.Context, cfg *Config) error {
	client, waiter := CreateClient(cfg)

	err := waiter.Run(ctx, func(ctx context.Context) error {
		return client.Run(ctx, func(ctx context.Context) error {
			if _, err := client.API().AuthLogOut(ctx); err != nil {
				return fmt.Errorf("calling auth logout: %w", err)
			}

			// Also delete stored session
			if err := NewSessionStorage().DeleteSession(); err != nil {
				fmt.Println("Failed to wipe session:", err)
			}

			fmt.Println("Successfully logged out from Telegram.")
			return nil
		})
	})
	if err != nil {
		return fmt.Errorf("logging out: %w", err)
	}
	return nil
}
