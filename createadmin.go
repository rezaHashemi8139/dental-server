package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	dentalv1 "dental-server/gen/dental/v1"
	"dental-server/internal/config"
	"dental-server/internal/database"
	"dental-server/internal/service"
)

// createAdmin creates a system administrator for the admin panel. It is the only
// way to create the first one; after that, administrators can also create others
// with AdminService.CreateAdmin.
//
//	dental-server create-admin -username admin -full-name "Site Admin"
//
// The password is read from standard input (one line), so it stays out of the
// shell history and the process list.
func createAdmin(args []string) error {
	flags := flag.NewFlagSet("create-admin", flag.ContinueOnError)
	username := flags.String("username", "", "login name: 3–32 letters, digits, '.', '_' or '-'")
	fullName := flags.String("full-name", "", "full name, 1–100 characters")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *username == "" || *fullName == "" {
		flags.Usage()
		return errors.New("-username and -full-name are required")
	}

	fmt.Fprint(os.Stderr, "Password (at least 8 characters, with a lowercase letter, an uppercase letter and a digit): ")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return fmt.Errorf("read password: %w", err)
	}
	password := strings.TrimRight(line, "\r\n")

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx := context.Background()
	db, err := database.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer db.Close()

	admin, err := service.AddFirstAdmin(ctx, db, &dentalv1.CreateAdminRequest{
		Username: *username, Password: password, FullName: *fullName,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Administrator %q created.\n", admin.GetUsername())
	return nil
}
