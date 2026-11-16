# Roadmap: campaigns and email dispatch

Personal branch `web-ui-and-proxy-fixes` only. Do not push, do not open a pull request, and do not merge into `main`.

This file is the handoff if work stops in the middle. Read it before editing. When a step is finished, change its status here in the same change.

## Status

- [x] Write this plan and `CHANGELOG.md` before feature code
- [x] SQLite tables for campaigns, job membership, mail settings, send log, dispatches
- [x] Domain types, template rendering, SMTP sender
- [x] Service rules: assign, save settings, start dispatch
- [x] Pages on the existing web layout, plus a campaign field on the job form
- [x] Tests for storage, templates, SMTP message shape, and the pages
- [x] `gofmt`, `go test ./web ./web/sqlite` (35 passed at the time; later mail-preview tests brought `./web` higher), and `go tool golangci-lint run ./web/...` (clean)
- [x] Consecutive paced dispatch, email checkbox default on, proxy only on Google search. See "Next". `go test ./web ./web/sqlite` 42 passed, `go tool golangci-lint run ./web/...` clean.

## Product rules

- A campaign is a named group of jobs that already exist in the web UI.
- One job is in at most one campaign. Assigning it again moves it.
- Deleting a campaign does not delete its jobs or their CSV files. It deletes the membership, the mail settings, the send log, and the dispatch history.
- Deleting a job removes it from its campaign.
- Mail settings belong to one campaign. Nothing is global.
- Settings fields: SMTP host, port, username, password, encryption, from email, from name, subject, body.
- Encryption is `starttls` (default, port 587), `ssl` (port 465), or `none`.
- An empty password on save keeps the password already stored. The form never receives the stored password.
- Subject and body are `text/template` values with missing keys treated as errors. Data passed in is only `Title`, `Email`, `Phone`, `Website`, `Address`, `Category`. Never pass the SMTP password into the template.
- Strip CR and LF from the rendered subject so a template cannot inject mail headers.
- Recipients come from the `emails` column of each job CSV (`gmaps.Entry` writes that column as comma-separated addresses). One message per address. Dedupe by lowercased address inside a campaign. The first row wins.
- Skip addresses already stored with status `sent` unless the user checks resend.
- A job with no CSV yet is counted as missing results, not as a failed send.
- Dispatch runs in the background after the page responds. Reloading the campaign page shows progress. A second dispatch for the same campaign is rejected while one is running.
- Use a background context for the send loop. The request context ends when the HTTP response is written.
- Do not log SMTP passwords or full message bodies.

## Data model

SQLite, same database as `jobs`, created from `web/sqlite` for databases that already exist. Enable `PRAGMA foreign_keys = ON` on the connection.

```text
campaigns(id TEXT PK, name TEXT NOT NULL, created_at INT, updated_at INT)
campaign_jobs(campaign_id TEXT, job_id TEXT, created_at INT,
              PRIMARY KEY (campaign_id, job_id),
              UNIQUE (job_id))
campaign_mail(campaign_id TEXT PK, smtp_host, smtp_port, smtp_username,
              smtp_password, from_email, from_name, subject, body,
              encryption, format, updated_at)
campaign_mail_log(campaign_id TEXT, email TEXT, status TEXT, error TEXT,
                  sent_at INT, PRIMARY KEY (campaign_id, email))
campaign_dispatches(id TEXT PK, campaign_id TEXT, status TEXT,
                    started_at INT, finished_at INT, sent INT, failed INT,
                    skipped INT, detail TEXT)
```

`status` on a dispatch is `running`, `ok`, or `failed`. `campaign_mail_log.status` is `sent` or `failed`. Only `sent` blocks the next dispatch.

If a `running` dispatch is older than two hours, mark it `failed` with detail `interrupted` and allow another run. The in-process lock still blocks a second run in the same process.

## UI

Follow `web/static/templates/index.html` and `web/static/css/main.css`. Same top bar, panels, fields, tables, and buttons. Do not invent a new visual system. UI copy stays English, like the rest of this app.

- Top nav on the job page and the campaign pages: Jobs (`/`), Campaigns (`/campaigns`). Mark the current page with `aria-current="page"`.
- `/campaigns`: left panel creates a campaign. Right panel lists campaigns with job count, whether mail settings look ready, and the latest dispatch status.
- `/campaigns/{id}`: left panel is the email form (sticky, same as the job form). Right panel renames the campaign, lists its jobs, assigns an existing job, starts dispatch, shows the last dispatches, and deletes the campaign.
- Job form: optional campaign select after the job name. Empty value means no campaign.
- Job list: under the job name, link the campaign when the job has one.
- While a dispatch is `running`, refresh the campaign page every 3 seconds with a meta refresh.
- Confirm before deleting a campaign. State that jobs and CSV files stay.

## HTTP

Register the templates in `web.New`. Patterns:

- `GET/POST /campaigns`
- `GET /campaigns/{id}`
- `POST /campaigns/{id}/rename`
- `POST /campaigns/{id}/delete`
- `POST /campaigns/{id}/jobs`
- `POST /campaigns/{id}/jobs/remove`
- `POST /campaigns/{id}/mail`
- `POST /campaigns/{id}/mail/test` sends one message to `test_to` and does not save
- `POST /campaigns/{id}/dispatch` with optional `resend=on`

Use `303` redirects and `notice` or `error` query values. Keep those messages short.

`CampaignStore` is a separate interface from `JobRepository`. The SQLite repo implements both. `Service` type-asserts the repo. If the repo has no campaign store, the job pages still work and the campaign list is empty.

## Files to add or edit

- `web/campaign.go` — types, validation, store interface
- `web/mail.go` — CSV recipients, templates, SMTP
- `web/campaign_service.go` — assign, save, dispatch
- `web/campaign_http.go` — handlers and page data
- `web/sqlite/campaigns.go` — schema and SQL
- `web/static/templates/campaigns.html`
- `web/static/templates/campaign.html`
- `web/web.go` — routes, templates, job create assigns the campaign
- `web/service.go` — fill campaign name on the job list
- `web/job.go` — `CampaignID` and `CampaignName` for display only. Do not persist them in the job JSON.
- `web/static/templates/index.html` — nav and campaign select
- `web/static/templates/job_rows.html` — campaign link
- `web/static/css/main.css` — style `select`, email, and password inputs like the other fields; current nav item uses `--ink`
- tests beside the new Go files

## How to check

```text
gofmt -w on each edited Go file
go test ./web ./web/sqlite
go tool golangci-lint run ./web/...
```

Both checks passed on this branch. The campaign pages were not opened in a browser. `go test` covers the index link, the campaign page (password stays out of the HTML), and the create redirect.

Build with `go build ./web/...` if the test binary is not enough.

## Known limits to leave unless asked

- Sends one SMTP connection per recipient. The paced dispatch below does not require reusing a connection.
- No CSRF token. The rest of this web UI does not use one.
- Password is plaintext in the local SQLite file, same trust level as proxy URLs already stored on the job.
- Server shutdown does not wait for an in-flight dispatch. A paced run can last hours. Leave that limit.
- No browser verification tool is required for this server-rendered UI if `go test` covers the pages. Say so if the pages were not opened in a browser.
- `campaign_mail.format` is `plain` (default) or `html`. `addMailFormatColumn` adds it on existing databases. Do not drop it.
- Compose watch on this machine was stopped with Ctrl+C. Do not restart it unless asked. Compose v5.7.0 rejects `up -d --watch`. Templates are `//go:embed static`, so after UI or mail-copy changes rebuild with `docker compose -f docker-compose.mysql.yaml up -d --build scraper`. `jobs.db` is the `./gmapsdata` volume. Mailpit stays on its own (`mailpit:1025`, no encryption, inbox http://127.0.0.1:8025). Windows `filerunner` TempDir tests fail with "arquivo já está sendo usado". Leave them.

## Next: consecutive sends, email default, proxy split

Done on this branch. Personal branch only. Do not push and do not open a PR.

What landed:

- `web/campaign_service.go` paces `runDispatch`. Tests inject `dispatchSleep` and `dispatchWait`. `TestRunDispatchPacesEveryAddress` sends 25 messages, including one failure, with 24 injected pauses and no real sleep.
- A live in-process run is not marked `interrupted` when it passes two hours. `DispatchRunning` stays true for that run so the page keeps refreshing. A running row with no live goroutine still expires after two hours.
- New job form `Email` defaults to true. `newJob.Data.Email` is still `r.Form.Get("email") == formChecked`. CLI `-email` is unchanged.
- `EmailExtractJob.DirectFetch` is true. `scrapemate.SkipsProxy` is what the fetchers check. Stealth skips `SetProxy`. The plain HTTP fetcher uses a client with `Proxy: nil`. The Playwright fetcher uses a second browser context launched with no proxy. Search and place jobs do not implement `DirectFetch`, so they keep the proxy.
- `go test github.com/gosom/scrapemate/adapters/fetchers/nethttp -run TestDirectFetchDoesNotUseProxiedClient` passed. `TestEmailExtractJobSkipsProxy` passed.

### Consecutive sends

Today `web/campaign_service.go` `runDispatch` already walks every not-yet-sent address in one background run (`context.Background()`), skips `campaign_mail_log` status `sent` unless resend is checked, retries `failed`, and rejects a second run with `ErrDispatchRunning`. Keep that. What is missing is the pace. One run must finish the whole remaining list. It must not stop after a single batch of 10. A later dispatch continues only the addresses that are still not `sent`.

Keep using `context.Background()`. The paced run is long. Do not tie it to the request context. The 2-hour rule (mark `running` as `failed` / `interrupted`) is only for a dead process. Do not mark a live in-process run interrupted because the wall clock passed 2 hours, and do not start a second dispatch beside it.

The campaign page already meta-refreshes every 3 seconds while a dispatch is `running`. The detail line should say when the run is waiting between batches, and it should keep the sent / failed counts. UI copy stays English.

### Email extraction defaults to on

The web job form currently leaves email extraction off.

- `web/web.go` index form default `Email: false`.
- `web/static/templates/index.html` checks the box only when `.Email` is set.
- The handler already stores `newJob.Data.Email = r.Form.Get("email") == formChecked`. An unchecked box must still turn extraction off.

Change the default and the template so a new job opens with the box checked. Do not change the CLI `-email` default in `runner/runner.go`. Do not change an API scrape default.

### Proxy only for Google search

The proxy is attached to the whole scrape today. `runner/jobs.go` passes the email flag and the proxies into `gmaps.NewGmapJob`. `runner/webrunner/webrunner.go` only logs that the job has a proxy.

Email extraction is a child job. `gmaps/place.go` builds `NewEmailJob` (`gmaps/emailjob.go` `EmailExtractJob`) when `ExtractEmail` is set and the website is valid. Google search and the place page must keep the proxy. The website fetch inside `EmailExtractJob` must not use it.

`scrapemateapp` has no proxy hook. The mate-level proxy is the likely reason the email job inherits it. Look in `third_party/scrapemate` and in how `runner/jobs.go` builds the mate before opting the email job out or giving it a direct client. Do not turn the proxy off for Google Maps.

### Batches of 10 with interleaved pauses

Reference: https://github.com/czarist/lg-prospector (v2.0.1.1). Read before changing the numbers.

Confirmed from source, not from the README alone:

- `app/core/config.py`: `mailman_batch_size = 12`, `mailman_interval_min_seconds = 120`, `mailman_interval_max_seconds = 300`, `mailman_intra_batch_min_seconds = 20`, `mailman_intra_batch_max_seconds = 75`.
- `app/services/mailman_service.py` `run_batch`: after each message in the batch, `random.uniform` between the intra min and max, then `asyncio.sleep`. No pause after the last message of the batch.
- `scripts/mailman.py`: after a non-empty batch, `random.uniform` between 120 and 300 seconds, then the next batch. Their batch of 12 is 4 niche + 4 generalist + 4 prestador.

Use that pause shape. Do not copy the rest.

- Batch size is 10. Send 10, pause 120–300 seconds, send the next 10, until the remaining list is done. A short final batch is fine.
- Between messages inside a batch, pause a random 20–75 seconds. No pause after the last message of a batch, and no between-batch pause after the final batch.
- Draw the random waits in the service and inject a sleeper. Unit tests assert call order and the wait calls. They must not sleep real minutes.
- Do not add their lane mix, the 4-day cooldown, Zoho, EspoCRM, IMAP, or bounce handling. Skip rules stay the ones in Product rules: only `sent` is skipped, unless resend is checked.
- Update the dispatch `detail` while waiting so the campaign page shows the pause.

### Files the next edit should touch

- `web/campaign_service.go` — pace `runDispatch`.
- `web/static/templates/campaign.html` — only if the existing dispatch detail is not enough to show the pause.
- `web/web.go` and `web/static/templates/index.html` — email checkbox default.
- `gmaps/emailjob.go`, `gmaps/place.go`, and whatever in `runner/` or `third_party/scrapemate` attaches the proxy.
- Tests beside those files.

### How to check this next slice

```text
gofmt -w on each edited Go file
go test ./web ./web/sqlite
go test on the gmaps and runner packages that changed
go tool golangci-lint run ./web/...
```

Lint the other packages only if they were edited. `golangci-lint` is `go tool golangci-lint` (v2.15.0), not on `PATH`.
