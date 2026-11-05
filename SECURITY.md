# Security

Please report vulnerabilities privately through GitHub's "Report a
vulnerability" button on this repository rather than in a public issue.

## What the bridge protects

- Only messages carrying Gmail's `SENT` label are relayed. Strangers can mail
  a token address, but they cannot make Gmail label it `SENT`.
- Token addresses are HMAC-SHA256 values keyed by `StateDir/token.key`; they
  cannot be guessed or forged without that key.
- Before sending, headers are rebuilt from an allowlist, token addresses and
  `(via …)` names are scrubbed from text parts, and a final leak check blocks
  any message that still mentions your Gmail address, a token address or a
  `(via …)` name, also after decoding HTML entities and URL escapes.
- `MaxSendsPerHour`, an idempotency record per Gmail message and `DryRun`
  limit the damage of a bug.

## What you must protect

- The config file holds your Google OAuth tokens and provider passwords.
  Never commit it; mount it read-only.
- `StateDir/token.key` must be backed up. If it is lost, every token address in
  existing mail stops resolving.
- The bridge uses the `gmail.modify` scope (full mail access with
  `DeleteOriginal = true`). Anyone with the tokens can read and modify your
  mailbox.
