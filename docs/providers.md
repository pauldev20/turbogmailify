# Provider settings

Starting points for `[[Imap]]` and `[Imap.Bridge.Smtp]`. Entries marked
*untested* come from provider documentation and have not been verified with
the bridge yet. Folder names vary; list them with any IMAP client and check
for the `\Junk` special-use flag. Most providers require an app password.

| Provider | IMAP | SMTP | Security | Junk folder | Status |
| --- | --- | --- | --- | --- | --- |
| AOL | `imap.aol.com:993` | `smtp.aol.com:465` | `tls` | `Bulk` | untested |
| Yahoo | `imap.mail.yahoo.com:993` | `smtp.mail.yahoo.com:465` | `tls` | `Bulk` | untested |
| GMX | `imap.gmx.net:993` | `mail.gmx.net:465` | `tls` | `Spamverdacht` | untested |
| Web.de | `imap.web.de:993` | `smtp.web.de:587` | `starttls` | `Spam` | untested |
| iCloud | `imap.mail.me.com:993` | `smtp.mail.me.com:587` | `starttls` | `Junk` | untested |
| Fastmail | `imap.fastmail.com:993` | `smtp.fastmail.com:465` | `tls` | `Spam` | untested |

Outlook.com and Hotmail no longer accept password logins for IMAP/SMTP and
need OAuth, which the bridge does not support.

Providers may delete accounts that look inactive. Check whether IMAP logins
count as activity for yours.
