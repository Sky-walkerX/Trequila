# Security Policy

## Reporting a Vulnerability
If you discover a security vulnerability in Trequila, please DO NOT disclose it publicly.
Instead, please email the maintainers or open a private security advisory on GitHub.

## Key Custody Model
Trequila is a Bring-Your-Own-Key (BYOK) application. User API keys are stored in the database.
- Keys are encrypted at rest using AES-256-GCM.
- The encryption key (`ENCRYPTION_KEY`) is stored outside the database as an environment variable.
- Keys are never logged and are decrypted only in-memory at the time of processing.
- The `/api/keys` endpoint masks keys and never returns them in plaintext.

Operators self-hosting Trequila are responsible for securing the `ENCRYPTION_KEY` and their database. Losing the `ENCRYPTION_KEY` will render all stored user keys unreadable.
