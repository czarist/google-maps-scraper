# Changelog

Personal branch `web-ui-and-proxy-fixes` only. Do not merge this file's notes into a release, do not open a pull request, and do not push to `main`.

## Unreleased

### Added

- Campaigns group existing web jobs. A job belongs to at most one campaign. Jobs with no campaign keep working as they do today.
- Each campaign has its own email dispatch settings: SMTP host, port, username, password, encryption (`starttls`, `ssl`, or `none`), from address, from name, subject, and plain-text body.
- Subject and body are templates. Placeholders: `{{.Title}}`, `{{.Email}}`, `{{.Phone}}`, `{{.Website}}`, `{{.Address}}`, `{{.Category}}`.
- Dispatch reads the `emails` column from each job CSV in the campaign, sends one message per address, and records what was sent so a later run skips addresses that already succeeded. A resend option sends to those addresses again.
- The password is stored in the local SQLite jobs database for this personal app. It is not written back into the HTML form and must not be logged.
- A campaign message can be plain text or HTML. An HTML file loads into the editor and can be changed before save. Preview renders the template with sample business data. Test SMTP connection checks the server, encryption, and login without sending mail.

- One campaign dispatch sends every contact that is not already `sent`. A later run continues whatever is left. Status `failed` can be retried. `sent` is still skipped unless resend is checked.
- The new-job form defaults "Collect emails" to on. Unchecking it still turns extraction off. The CLI `-email` flag stays off.
- The job proxy is used only for Google Maps search and the place page. The website visit that extracts emails (`EmailExtractJob`) goes out direct.
- The web UI has a light theme and a dark theme. The choice is remembered in the browser. With no saved choice it follows the system theme. Light stays the printed-map palette.
- Dispatch sends in batches of 10 and keeps going until that run's list is done. Between messages in a batch it waits a random 20–75 seconds. Between batches it waits a random 120–300 seconds. There is no pause after the last message. The campaign page shows the wait. The shape comes from [lg-prospector](https://github.com/czarist/lg-prospector) mailman (`mailman_intra_batch_*` 20–75s, `mailman_interval_*` 120–300s). The batch size here is 10, not their 12, and there is no niche/generalist/prestador split.

### Out of scope

- Pushing, opening a PR, or changing `main`.
- A shared mailbox for every campaign. Settings are per campaign on purpose.
- Attachments, open tracking, and a public API for campaigns.
- Zoho, EspoCRM, IMAP, bounce handling, a 4-day resend cooldown, and the lg-prospector lane mix. SMTP on this branch stays the sender.
