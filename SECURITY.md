# Security Policy

## Supported versions

Trequila is pre-1.0. Security fixes are made on `main`.

## Reporting a vulnerability

Please **do not** open a public issue. Report privately through GitHub:
<https://github.com/Sky-walkerX/Trequila/security/advisories/new>

Include what you found, how to reproduce it, and the impact you see. You can expect an acknowledgement within
a few days. Please give us a reasonable time to fix the problem before disclosing it.

## Key custody model

Trequila is bring-your-own-key: users store their own LLM API keys.

- Keys are encrypted at rest with AES-256-GCM.
- The encryption key (`ENCRYPTION_KEY`) lives outside the database, in the environment.
- Keys are decrypted only in memory, only when a request needs them, and are never logged.
- The `/api/keys` endpoints mask keys and never return plaintext.
- The server refuses to start if `JWT_SECRET` is shorter than 32 bytes or `ENCRYPTION_KEY` is invalid.

Operators who self-host are responsible for protecting `ENCRYPTION_KEY` and the database. Losing
`ENCRYPTION_KEY` makes every stored user key unreadable.
