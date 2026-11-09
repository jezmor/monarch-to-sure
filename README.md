# Monarch to Sure

Moves your accounts, transactions and daily balance history from
[Monarch](https://www.monarchmoney.com) into a self-hosted
[Sure](https://github.com/we-promise/sure) server, through Sure's API.

It comes in two runtimes that do exactly the same thing: a **Go** binary
with nothing to install, and a **Python** script that needs only Python 3.9 or
later. Both include a browser wizard, take the same commands and options, and
write byte-identical import files.

This is an independent project, not affiliated with Monarch or with Sure.

## Quick start

1. **Export from Monarch.** Download your balance history and your
   transactions as CSV files.
2. **Create a Sure API key.** In Sure, go to **Settings → API keys** and
   create a key with read/write access. Log in as the user you're importing
   for: the accounts go into that user's family.
3. **Get this tool**, with [Go](https://go.dev/dl/) 1.22 or later:

   ```sh
   go install github.com/jezmor/monarch-to-sure/go/cmd/monarch-to-sure@latest
   ```

   That puts `monarch-to-sure` in `$(go env GOPATH)/bin`. Or, with Python
   instead:

   ```sh
   git clone https://github.com/jezmor/monarch-to-sure.git
   cd monarch-to-sure
   ```

4. **Run the wizard:**

   ```sh
   monarch-to-sure wizard                 # Go
   python3 python/monarch_to_sure.py wizard  # Python
   ```

   It opens in your browser. Connect with your Sure address and API key,
   choose the two exports, check each account, review, and import. Keep the
   terminal open until it's done, then press Ctrl+C.

To start over in Sure, use **Settings → Profile → Reset** for that user, which
deletes the family's financial data but keeps the user and its API keys.

## Requirements

- Go 1.22 or later to build the Go runtime, or Python 3.9 or later for the
  Python one. Neither needs any other packages.
- A Sure server with the import sessions API (`/api/v1/import_sessions`).
  Tested against the `ghcr.io/we-promise/sure:stable` image at commit
  `605d002`.

## Layout

The repo is organized by runtime:

| Path | What |
| --- | --- |
| `go/` | Go runtime: `cmd/monarch-to-sure` (the command) and `internal/` (`importer` converts the exports, `sure` talks to the API, `wizard` serves the page) |
| `python/` | Python runtime: `monarch_to_sure.py` (the command and converter) and `wizard.py` |
| `web/wizard.html` | The wizard page. Both runtimes serve this one file; Go embeds it in the binary |
| `scripts/parity.sh` | Converts the same exports with both runtimes and checks the import files are identical |

The examples below use the Go command, `monarch-to-sure`. For Python, use
`python3 python/monarch_to_sure.py` with the same arguments.

## What it imports

| Monarch | Sure |
| --- | --- |
| Accounts in either export | Manual accounts, typed from your `accounts.json` |
| Transactions | Transactions with merchant, category, tags and notes |
| Daily balance history | Balance valuations, only where transactions don't already explain the balance |
| `Transfer` and `Credit Card Payment` categories | `funds_movement` and `cc_payment` transaction kinds, with no category |

Sure computes balances from transactions plus valuations. The converter walks
each account day by day and adds a valuation only where Monarch's balance
differs from what the transactions alone would give, so Sure shows the same
history without a valuation every day.

Not imported: pending transactions (unless `--include-pending` is passed),
`Original Statement` (used as the transaction name only when `Merchant` is
blank), and `Owner`, `Reviewed`, `Reviewed At` and `Reviewed By`.

## The wizard

```sh
monarch-to-sure wizard
```

It opens a page on this computer that walks through the whole import:
connect to Sure with an API key, choose the Monarch exports, check each
account's type and whether it's closed, review what will be created, and
import. It runs the same conversion and upload as the commands below. The
exports stay in memory and aren't saved anywhere, and the page only answers
on `127.0.0.1` to the link printed in the terminal.

Leave the transactions export out to import balance history only. To do that
for some accounts only, untick their **Transactions** box. **Balance today**
starts at Monarch's latest balance; change it if that's out of date, and Sure
records the new balance as of today. In `accounts.json` these are
`"transactions": false` and `"current_balance"`, with amounts owed entered as
positive numbers.

Options: `--port` (default 8765), `--no-browser` to print the link instead of
opening it, and `--url` and `--api-key` (or `SURE_URL` and `SURE_API_KEY`) to
skip the connect step.

## Command line

The same import can be run step by step, which lets you keep and edit the
account config and inspect the import file before uploading it.

1. In Monarch, export transactions and balance history as CSV files.
2. Create an API key in Sure under **Settings → API keys**, with read/write access.
3. Write the account config:

   ```sh
   monarch-to-sure init-config \
     --balances Balances.csv --transactions Transactions.csv -o accounts.json
   ```

   Leave out `--transactions`, here and in `convert`, to import balance
   history only. Every change in balance then becomes a balance valuation.

   It lists every account with its last activity: the later of its last
   transaction and the last day its balance changed. It then asks which
   accounts are closed and which to skip. Answer with numbers and ranges such
   as `1,4-6`, or `none`, or press Enter to accept the suggestion. Accounts
   with no activity in the last 180 days are suggested as closed, but
   inactivity doesn't prove an account was closed, so check each one. See
   [Closed accounts](#closed-accounts).

   Pass `--no-input` to skip the questions. Every account is then imported as
   open and active.

   Then open `accounts.json` and check each account's `type`. The CSVs don't
   say what type each account is, so the tool guesses: mostly-negative balances
   become `CreditCard` (or `Loan` when there are no transactions), other
   accounts with transactions become `Depository`, and the rest become
   `Investment`. A negative balance alone can't tell a credit card from a
   loan, so check every type. You can also change `closed`, `status`
   (`active` or `disabled`) and `include` there, and optionally set `subtype`
   and `currency`.

4. Build the import file. `convert` reads your existing categories and
   merchants from Sure (see [Existing data](#existing-data)), so it needs the
   server too:

   ```sh
   export SURE_URL=https://sure.example.com SURE_API_KEY=...
   monarch-to-sure convert \
     --balances Balances.csv --transactions Transactions.csv \
     --config accounts.json -o sure-import.ndjson
   ```

   Pass `--offline` instead to build a file just for inspection.

5. Upload it:

   ```sh
   monarch-to-sure upload sure-import.ndjson
   ```

   The upload is split into chunks of up to 4 MB and imported as a single Sure
   import session. Sure then syncs the family to calculate balances.

## Existing data

The import adds new accounts and leaves the accounts, transactions, categories
and merchants already in Sure unchanged:

- Every Monarch account becomes a new Sure account. Existing accounts are
  never matched or updated.
- Monarch categories with the same name as an existing Sure category reuse it.
  `convert` copies that category's current color and icon into the file,
  because Sure's importer rewrites both from the file.
- Monarch merchants with the same name as an existing Sure merchant are not
  sent. The importer would replace their color and logo, and the API doesn't
  expose those to copy. Those transactions keep the merchant name but aren't
  linked to the merchant.
- Before uploading anything, `upload` checks the server again and stops if the
  file would change an existing category or merchant.

After the import Sure syncs the family. Its automatic transfer matching may
suggest transfers between imported and existing transactions. These stay
pending until you confirm or reject them.

## Re-running

The import session ID comes from a hash of the NDJSON file. Uploading the same
file again resumes an interrupted or failed import, and does nothing if that
import already finished.

A changed file is a new import and would create duplicate accounts. To guard
against this, `upload` stops if Sure already has an account with the same name
as one being imported. Pass `--allow-existing-accounts` to import anyway.

## Closed accounts

The tool only treats an account as closed when you say so, either at the
prompt or with `"closed": true` in `accounts.json`. A closed account:

- gets a zero balance the day after Monarch's last balance for it, so Sure
  doesn't keep showing its final balance forever
- is imported disabled: hidden from current balances, but still part of net
  worth history

An account that isn't closed keeps its last Monarch balance, even when its
history ends before the rest of the export. To hide an open account without
zeroing it, set `"status": "disabled"` and leave `closed` as `false`.

## Privacy

Your exports never leave your computer except to go to the Sure server you
connect to. The wizard keeps them in memory. The command line writes
`accounts.json` (account names and balance summaries) and the NDJSON import
file (everything) wherever you point `-o`, so delete those when you're done.
This repo's `.gitignore` keeps CSV, NDJSON and `accounts.json` files out of
commits.

## Tests

```sh
go test ./...                            # Go
(cd python && python3 -m unittest)       # Python
scripts/parity.sh Balances.csv Transactions.csv
```

The parity check converts your own exports with both runtimes and compares the
results. Its files stay in a temporary directory that it deletes afterwards.

## License

[AGPL-3.0](LICENSE), the same license as Sure.
