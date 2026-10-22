package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/medonomator/files-service/internal/apikey"
	"github.com/medonomator/files-service/internal/domain"
	"github.com/medonomator/files-service/internal/postgres"
)

const usage = `usage: clients <command> [flags]

commands:
  create   -slug hs -name "Home Sharing" -types "image/*,video/*,application/pdf" -max-file-mb 2048 -quota-gb 100 [-rate 120]
  key      -slug hs              issue an extra key (for rotation)
  revoke   -key fsk_XXXXXXXX     revoke a key by its display prefix
  disable  -slug hs
  enable   -slug hs
  list`

func main() {
	if len(os.Args) < 2 {
		fail(errors.New(usage))
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		fail(errors.New("DATABASE_URL is required"))
	}

	ctx := context.Background()

	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		fail(err)
	}

	defer pool.Close()

	store := postgres.New(pool)
	command, args := os.Args[1], os.Args[2:]

	switch command {
	case "create":
		err = create(ctx, store, args)
	case "key":
		err = issueKey(ctx, store, args)
	case "revoke":
		err = revoke(ctx, store, args)
	case "disable", "enable":
		err = toggle(ctx, store, args, command == "disable")
	case "list":
		err = list(ctx, store)
	default:
		err = errors.New(usage)
	}

	if err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

func create(ctx context.Context, store *postgres.Store, args []string) error {
	flags := flag.NewFlagSet("create", flag.ExitOnError)
	slug := flags.String("slug", "", "client slug, becomes the object prefix")
	name := flags.String("name", "", "human readable name")
	types := flags.String("types", "", "comma separated content types or family/* patterns")
	maxFileMB := flags.Int64("max-file-mb", 100, "max single file size in MB")
	quotaGB := flags.Int64("quota-gb", 10, "total storage quota in GB")
	rateLimit := flags.Int("rate", 120, "requests per minute")
	_ = flags.Parse(args)

	if !domain.ValidSlug(*slug) {
		return errors.New("slug must be 2-32 chars of a-z, 0-9 and -")
	}

	if strings.TrimSpace(*name) == "" {
		return errors.New("name is required")
	}

	var allowed []string
	for _, pattern := range strings.Split(*types, ",") {
		pattern = domain.NormalizeContentType(pattern)
		if pattern == "" {
			continue
		}

		if !domain.ValidTypePattern(pattern) {
			return fmt.Errorf("unknown content type pattern %q", pattern)
		}

		allowed = append(allowed, pattern)
	}

	if len(allowed) == 0 {
		return errors.New("types is required")
	}

	if *maxFileMB < 1 || *quotaGB < 1 || *rateLimit < 1 {
		return errors.New("max-file-mb, quota-gb and rate must be positive")
	}

	client, err := store.CreateClient(ctx, domain.Client{
		Slug:               *slug,
		Name:               strings.TrimSpace(*name),
		AllowedTypes:       allowed,
		MaxFileBytes:       *maxFileMB << 20,
		QuotaBytes:         *quotaGB << 30,
		RateLimitPerMinute: *rateLimit,
	})
	if err != nil {
		return err
	}

	return printNewKey(ctx, store, client)
}

func issueKey(ctx context.Context, store *postgres.Store, args []string) error {
	flags := flag.NewFlagSet("key", flag.ExitOnError)
	slug := flags.String("slug", "", "client slug")
	_ = flags.Parse(args)

	client, err := store.ClientBySlug(ctx, *slug)
	if errors.Is(err, domain.ErrNotFound) {
		return fmt.Errorf("client %q not found", *slug)
	}

	if err != nil {
		return err
	}

	return printNewKey(ctx, store, client)
}

func printNewKey(ctx context.Context, store *postgres.Store, client domain.Client) error {
	issued, err := apikey.Generate()
	if err != nil {
		return err
	}

	if err := store.AddKey(ctx, client.ID, issued.Hash, issued.Display); err != nil {
		return err
	}

	fmt.Printf("client: %s (%s)\napi key: %s\nThe key is shown once, only its hash is stored.\n", client.Slug, client.Name, issued.Plain)

	return nil
}

func revoke(ctx context.Context, store *postgres.Store, args []string) error {
	flags := flag.NewFlagSet("revoke", flag.ExitOnError)
	display := flags.String("key", "", "key display prefix as shown by list")
	_ = flags.Parse(args)

	if err := store.RevokeKey(ctx, *display); errors.Is(err, domain.ErrNotFound) {
		return fmt.Errorf("active key %q not found", *display)
	} else if err != nil {
		return err
	}

	fmt.Println("revoked", *display)

	return nil
}

func toggle(ctx context.Context, store *postgres.Store, args []string, disabled bool) error {
	flags := flag.NewFlagSet("toggle", flag.ExitOnError)
	slug := flags.String("slug", "", "client slug")
	_ = flags.Parse(args)

	if err := store.SetClientDisabled(ctx, *slug, disabled); errors.Is(err, domain.ErrNotFound) {
		return fmt.Errorf("client %q not found", *slug)
	} else if err != nil {
		return err
	}

	fmt.Println("updated", *slug)

	return nil
}

func list(ctx context.Context, store *postgres.Store) error {
	summaries, err := store.ListClients(ctx)
	if err != nil {
		return err
	}

	writer := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "SLUG\tNAME\tSTATE\tFILES\tUSED MB\tQUOTA GB\tMAX FILE MB\tRATE/MIN\tTYPES\tKEYS")

	for _, summary := range summaries {
		client := summary.Client
		state := "active"
		if summary.Disabled {
			state = "disabled"
		}

		fmt.Fprintf(writer, "%s\t%s\t%s\t%d\t%d\t%d\t%d\t%d\t%s\t%s\n",
			client.Slug, client.Name, state, summary.FileCount, summary.UsedBytes>>20, client.QuotaBytes>>30,
			client.MaxFileBytes>>20, client.RateLimitPerMinute,
			strings.Join(client.AllowedTypes, ","), strings.Join(summary.ActiveKeys, ","),
		)
	}

	return writer.Flush()
}
