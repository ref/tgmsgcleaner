package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"

	"context"

	tea "charm.land/bubbletea/v2"
	"github.com/anik1ng/tgmsgcleaner/internal/config"
	"github.com/anik1ng/tgmsgcleaner/internal/phonenum"
	tg "github.com/anik1ng/tgmsgcleaner/internal/telegram"
	"github.com/anik1ng/tgmsgcleaner/internal/tui"
)

func main() {
	resetFlag := flag.Bool("reset", false, "Reset all settings and accounts")
	flag.Parse()

	if *resetFlag {
		if err := config.Reset(); err != nil {
			fmt.Printf("failed to reset: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("All settings and accounts have been reset.")
		return
	}

	cfg, err := config.LoadGlobal()
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			fmt.Printf("failed to load config: %v\n", err)
			os.Exit(1)
		}
		reader := bufio.NewReader(os.Stdin)
		fmt.Println("First run setup. Get api_id and api_hash from https://my.telegram.org/apps")
		fmt.Print("Enter api_id: ")
		apiIDStr, _ := reader.ReadString('\n')
		apiIDStr = strings.TrimSpace(apiIDStr)
		apiID, err := strconv.Atoi(apiIDStr)
		if err != nil {
			fmt.Printf("invalid api_id: %v\n", err)
			os.Exit(1)
		}
		fmt.Print("Enter api_hash: ")
		apiHash, _ := reader.ReadString('\n')
		apiHash = strings.TrimSpace(apiHash)
		if apiHash == "" {
			fmt.Println("api_hash cannot be empty")
			os.Exit(1)
		}
		cfg = config.GlobalConfig{APIID: apiID, APIHash: apiHash}
		if err := config.SaveGlobal(cfg); err != nil {
			fmt.Printf("failed to save config: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("Config saved.")
	}

	addAccount := false
	for {
		phone, err := pickAccount(addAccount)
		if err != nil {
			fmt.Printf("error: %v\n", err)
			os.Exit(1)
		}
		if phone == "" {
			return // user quit the account picker — clean exit
		}

		// An account added in this run is kept only if the sign-in goes through,
		// so a mistyped number or a cancelled login leaves no empty account in
		// the list.
		accounts, err := config.ListAccounts()
		if err != nil {
			fmt.Printf("failed to list accounts: %v\n", err)
			os.Exit(1)
		}
		keepAccount := slices.Contains(accounts, phone)

		sessionPath, err := config.SessionPath(phone)
		if err != nil {
			fmt.Printf("failed to get session path: %v\n", err)
			os.Exit(1)
		}

		exitAction := tui.ExitQuit
		client := tg.NewClient(cfg.APIID, cfg.APIHash, sessionPath)
		runErr := client.Run(context.Background(), func(ctx context.Context) error {
			isAuth, err := tg.IsAuthorized(ctx, client)
			if err != nil {
				return fmt.Errorf("check auth status: %w", err)
			}

			app := tui.NewApp(client, ctx, phone, sessionPath, isAuth)
			finalModel, err := tea.NewProgram(app).Run()
			if !keepAccount {
				// A failed check counts as signed in: never delete a session
				// that may be live.
				signedIn, authErr := tg.IsAuthorized(ctx, client)
				keepAccount = authErr != nil || signedIn
			}
			if err != nil {
				return fmt.Errorf("run TUI: %w", err)
			}
			if final, ok := finalModel.(tui.App); ok {
				exitAction = final.ExitAction
			}
			return nil
		})
		if !keepAccount {
			// After Run returns, so the stopped client cannot write the
			// session file again.
			if err := config.DeleteAccount(phone); err != nil {
				fmt.Printf("failed to remove the unused account: %v\n", err)
			}
		}
		if runErr != nil {
			fmt.Printf("error: %v\n", runErr)
			os.Exit(1)
		}

		switch exitAction {
		case tui.ExitAdd:
			addAccount = true
			continue
		case tui.ExitSwitch:
			addAccount = false
			continue
		default:
			return
		}
	}
}

// askPhone reads a phone number from stdin until it gets a valid one, and
// returns "" when the user enters nothing. A number that already has an account
// returns that account's name, so one number never gets two accounts.
func askPhone(accounts []string) (string, error) {
	hint := "leave empty to quit"
	if len(accounts) > 0 {
		hint = "leave empty to go back"
	}
	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Printf("Enter phone number (e.g. +79001234567, %s): ", hint)
		line, readErr := reader.ReadString('\n')
		input := strings.TrimSpace(line)
		if input == "" {
			return "", nil
		}
		phone, err := phonenum.Normalize(input)
		if err != nil {
			if readErr != nil {
				return "", err // stdin is closed; asking again would loop forever
			}
			fmt.Printf("Invalid input: %v\n", err)
			continue
		}
		for _, account := range accounts {
			if phonenum.Same(account, phone) {
				return account, nil
			}
		}
		return phone, nil
	}
}

func pickAccount(forceNew bool) (string, error) {
	for {
		accounts, err := config.ListAccounts()
		if err != nil {
			return "", fmt.Errorf("list accounts: %w", err)
		}

		if forceNew || len(accounts) == 0 {
			phone, err := askPhone(accounts)
			if err != nil {
				return "", err
			}
			if phone == "" && len(accounts) > 0 {
				forceNew = false // nothing entered: back to the account list
				continue
			}
			return phone, nil // empty with no accounts — caller treats it as a clean exit
		}

		app := tui.NewPickerApp(accounts)
		finalModel, err := tea.NewProgram(app).Run()
		if err != nil {
			return "", fmt.Errorf("account picker: %w", err)
		}
		final, ok := finalModel.(tui.App)
		if !ok {
			return "", nil
		}

		switch final.ExitAction {
		case tui.ExitAdd:
			forceNew = true
		case tui.ExitDelete:
			if err := config.DeleteAccount(final.Selected); err != nil {
				return "", err
			}
			// Loop to reopen the picker with the updated list.
		default:
			return final.Selected, nil // empty when the user quit the picker — caller treats it as a clean exit
		}
	}
}
