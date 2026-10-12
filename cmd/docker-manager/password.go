package main

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/crypto/bcrypt"
	"golang.org/x/term"
)

// cmdPassword prints the bcrypt hash for password_hash. Prompts go to stderr, so
// `docker-manager password > hash` works.
func cmdPassword() error {
	pw, err := readNewPassword()
	if err != nil {
		return err
	}
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	fmt.Println(string(h))
	if term.IsTerminal(int(os.Stdout.Fd())) {
		fmt.Fprintln(os.Stderr, "Put it in config.yaml as password_hash (quoted) and restart Dockira; existing logins end.")
	}
	return nil
}

// cmdGenKey prints a new secret_key. Changing it ends all logins and makes stored channel
// secrets unreadable, so keep it with the backups.
func cmdGenKey() error {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return err
	}
	fmt.Println(base64.StdEncoding.EncodeToString(b))
	return nil
}

// readNewPassword asks twice on a terminal, or reads one line from stdin (scripts).
func readNewPassword() (string, error) {
	var pw string
	if fd := int(os.Stdin.Fd()); term.IsTerminal(fd) {
		ask := func(prompt string) (string, error) {
			fmt.Fprint(os.Stderr, prompt)
			b, err := term.ReadPassword(fd)
			fmt.Fprintln(os.Stderr)
			return string(b), err
		}
		var err error
		if pw, err = ask("New UI password: "); err != nil {
			return "", err
		}
		again, err := ask("Repeat: ")
		if err != nil {
			return "", err
		}
		if again != pw {
			return "", errors.New("the passwords differ")
		}
	} else {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}
		pw = strings.TrimRight(line, "\r\n")
	}
	if pw == "" {
		return "", errors.New("the password is empty")
	}
	return pw, nil
}
