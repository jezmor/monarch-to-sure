// Command monarch-to-sure moves accounts, transactions and balance history
// from Monarch's CSV exports into a Sure server.
//
//	wizard       do the whole import in your browser
//	init-config  inspect the exports and write an editable account config
//	convert      build the Sure NDJSON file (review it before uploading)
//	upload       send the NDJSON file to a Sure server
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/jezmor/monarch-to-sure/go/internal/importer"
	"github.com/jezmor/monarch-to-sure/go/internal/sure"
	"github.com/jezmor/monarch-to-sure/go/internal/wizard"
)

const usage = `Move accounts, transactions and balance history from Monarch to Sure.

Usage:
  monarch-to-sure wizard [--port 8765] [--no-browser] [--url URL --api-key KEY]
  monarch-to-sure init-config --balances FILE [--transactions FILE] [-o accounts.json] [--no-input]
  monarch-to-sure convert --balances FILE [--transactions FILE] [--config accounts.json]
                          [-o sure-import.ndjson] [--include-pending] [--offline] [--url URL --api-key KEY]
  monarch-to-sure upload FILE [--allow-existing-accounts] [--url URL --api-key KEY]

--url and --api-key default to SURE_URL and SURE_API_KEY. Leave out
--transactions to import balance history only.
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Print(usage)
		return nil
	}
	command, args := args[0], args[1:]
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	server := func() (*string, *string) {
		return fs.String("url", os.Getenv("SURE_URL"), "Sure server URL"),
			fs.String("api-key", os.Getenv("SURE_API_KEY"), "read/write API key")
	}

	switch command {
	case "wizard":
		port := fs.Int("port", 8765, "local port; 0 picks a free one")
		noBrowser := fs.Bool("no-browser", false, "print the link instead of opening it")
		url, apiKey := server()
		if _, err := parse(fs, args); err != nil {
			return err
		}
		return wizard.Run(*port, !*noBrowser, *url, *apiKey)

	case "init-config":
		balances := fs.String("balances", "", "Monarch balances export")
		transactions := fs.String("transactions", "", "Monarch transactions export; omit to import balance history only")
		output := fs.String("o", "accounts.json", "where to write the config")
		noInput := fs.Bool("no-input", false, "don't ask which accounts are closed or skipped")
		if _, err := parse(fs, args); err != nil {
			return err
		}
		return initConfig(*balances, *transactions, *output, !*noInput && isTerminal(os.Stdin))

	case "convert":
		balances := fs.String("balances", "", "Monarch balances export")
		transactions := fs.String("transactions", "", "Monarch transactions export; omit to import balance history only")
		configPath := fs.String("config", "accounts.json", "account config from init-config")
		output := fs.String("o", "sure-import.ndjson", "where to write the import file")
		includePending := fs.Bool("include-pending", false, "also import pending transactions")
		offline := fs.Bool("offline", false, "build without checking the server's existing categories and merchants; upload will refuse it if they clash")
		url, apiKey := server()
		if _, err := parse(fs, args); err != nil {
			return err
		}
		var existing *importer.Existing
		if !*offline {
			if *url == "" || *apiKey == "" {
				return errors.New("convert needs --url and --api-key (or SURE_URL and SURE_API_KEY), or --offline")
			}
			var err error
			if existing, err = sure.NewClient(*url, *apiKey).ExistingRecords(); err != nil {
				return err
			}
		}
		return convert(*balances, *transactions, *configPath, *output, *includePending, existing)

	case "upload":
		allowExisting := fs.Bool("allow-existing-accounts", false, "import even if Sure has accounts with the same names")
		url, apiKey := server()
		positional, err := parse(fs, args)
		if err != nil {
			return err
		}
		if len(positional) != 1 {
			return errors.New("upload needs the NDJSON file to send")
		}
		if *url == "" || *apiKey == "" {
			return errors.New("upload needs --url and --api-key (or SURE_URL and SURE_API_KEY)")
		}
		return upload(positional[0], sure.NewClient(*url, *apiKey), *allowExisting)
	}
	return fmt.Errorf("unknown command %q\n\n%s", command, usage)
}

// parse lets positional arguments appear before, between or after flags.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return positional, nil
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func readExports(balancesPath, transactionsPath string) (importer.Balances, []importer.Row, error) {
	if balancesPath == "" {
		return nil, nil, errors.New("--balances is required")
	}
	rows, err := readRows(balancesPath)
	if err != nil {
		return nil, nil, err
	}
	balances, err := importer.ParseBalances(rows)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", balancesPath, err)
	}
	var transactions []importer.Row
	if transactionsPath != "" {
		if transactions, err = readRows(transactionsPath); err != nil {
			return nil, nil, err
		}
	}
	return balances, transactions, nil
}

func readRows(path string) ([]importer.Row, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	rows, err := importer.ReadRows(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return rows, nil
}

func initConfig(balancesPath, transactionsPath, output string, interactive bool) error {
	balances, transactions, err := readExports(balancesPath, transactionsPath)
	if err != nil {
		return err
	}
	config, exportEnd, err := importer.InspectAccounts(balances, transactions)
	if err != nil {
		return err
	}
	if interactive {
		in := bufio.NewReader(os.Stdin)
		ask := func(prompt string) (string, error) {
			fmt.Print(prompt)
			line, err := in.ReadString('\n')
			if err != nil && line == "" {
				return "", err
			}
			return line, nil
		}
		if err := importer.ReviewAccounts(&config, exportEnd, ask, func(s string) { fmt.Println(s) }); err != nil {
			return err
		}
	}
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(output, append(data, '\n'), 0o600); err != nil {
		return err
	}
	fmt.Printf("\nWrote %s with %d accounts. Review the types before converting.\n", output, len(config.Accounts))
	return nil
}

func convert(balancesPath, transactionsPath, configPath, output string, includePending bool, existing *importer.Existing) error {
	balances, transactions, err := readExports(balancesPath, transactionsPath)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		return err
	}
	config, err := importer.LoadConfig(data)
	if err != nil {
		return fmt.Errorf("%s: %w", configPath, err)
	}
	records, stats, err := importer.Convert(balances, transactions, config, importer.Options{
		IncludePending: includePending, Existing: existing,
	})
	if err != nil {
		return err
	}
	lines, err := importer.NDJSONLines(records)
	if err != nil {
		return err
	}
	if err := os.WriteFile(output, []byte(strings.Join(lines, "")), 0o600); err != nil {
		return err
	}
	fmt.Printf("Wrote %s\n", output)
	keys := make([]string, 0, len(stats))
	for k := range stats {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("  %s: %d\n", k, stats[k])
	}
	return nil
}

func upload(path string, client sure.API, allowExisting bool) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lines := strings.SplitAfter(string(data), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	session, err := sure.Upload(lines, client, allowExisting, 5*time.Second, func(s string) { fmt.Println(s) })
	if err != nil {
		return err
	}
	fmt.Printf("Import session %s: %s\n", session.ID(), session.Status())
	summary, _ := json.MarshalIndent(session["summary"], "", "  ")
	fmt.Println(string(summary))
	if session.Status() != "complete" {
		detail, _ := json.MarshalIndent(session["error"], "", "  ")
		return fmt.Errorf("the import didn't complete: %s", detail)
	}
	return nil
}
