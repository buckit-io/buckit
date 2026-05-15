# BuckIt Authentication

This document describes how authentication works for the two primary client interfaces: the **Console** (web UI) and the **mc CLI**.

---

## Console Login (Web UI)

The Console is a web application that proxies requests to the BuckIt server. It does not store credentials in the browser — instead it uses encrypted session cookies backed by STS temporary credentials.

### Flow Overview

```
┌──────────┐         ┌──────────────┐         ┌──────────────┐
│  Browser │         │ Console API  │         │ BuckIt Server│
└────┬─────┘         └──────┬───────┘         └──────┬───────┘
     │                      │                        │
     │ 1. GET /api/v1/login │                        │
     │─────────────────────>│                        │
     │   (login strategy)   │                        │
     │<─────────────────────│                        │
     │                      │                        │
     │ 2. POST /api/v1/login│                        │
     │   {accessKey,        │                        │
     │    secretKey}        │                        │
     │─────────────────────>│                        │
     │                      │ 3. STS AssumeRole      │
     │                      │───────────────────────>│
     │                      │   (or LDAP auth)       │
     │                      │<───────────────────────│
     │                      │   {tempAccessKey,      │
     │                      │    tempSecretKey,      │
     │                      │    sessionToken}       │
     │                      │                        │
     │                      │ 4. Encrypt STS creds   │
     │                      │    into session token  │
     │                      │                        │
     │ 5. Set-Cookie: token │                        │
     │<─────────────────────│                        │
     │   (HttpOnly, Secure) │                        │
     │                      │                        │
     │ 6. Subsequent API    │                        │
     │    requests w/ cookie│                        │
     │─────────────────────>│                        │
     │                      │ 7. Decrypt cookie →    │
     │                      │    recover STS creds → │
     │                      │    call BuckIt API     │
     │                      │───────────────────────>│
     │                      │<───────────────────────│
     │<─────────────────────│                        │
```

### Step-by-Step

1. **Login strategy discovery** — Frontend calls `GET /api/v1/login` to determine the login method:
   - `"form"` — username/password form (default)
   - `"redirect"` — SSO via OpenID Connect (one or more IDP providers configured)

2. **Credential submission** — User submits `accessKey` + `secretKey` via `POST /api/v1/login`.

3. **STS credential exchange** — Console backend calls BuckIt server's STS AssumeRole endpoint using the provided credentials. If LDAP is enabled, it first attempts LDAP authentication, falling back to STS if the user is not found in LDAP.

4. **Session token creation** — The returned temporary STS credentials (accessKeyID, secretAccessKey, sessionToken) are encrypted into a session token using:
   - **Key derivation**: PBKDF2 (SHA-1, 4096 iterations, 32-byte key) from `CONSOLE_PBKDF_PASSPHRASE` + `CONSOLE_PBKDF_SALT`
   - **Encryption**: AES-GCM (if hardware AES available) or ChaCha20-Poly1305
   - **Format**: `algorithm_byte | 16-byte IV | 12-byte nonce | ciphertext` → base64 encoded

5. **Cookie set** — Session token is stored as an HttpOnly cookie:
   - **Name**: `token`
   - **Path**: `/`
   - **HttpOnly**: true (not accessible to JavaScript)
   - **Secure**: true (only if TLS is enabled)
   - **SameSite**: Lax
   - **MaxAge**: STS duration (default 12 hours, configurable via `CONSOLE_STS_DURATION`)

6. **Subsequent requests** — Browser sends the cookie automatically with every request.

7. **Request authentication** — Console backend decrypts the cookie to recover STS credentials, then uses them to make authenticated calls to the BuckIt server on behalf of the user.

### OAuth2/OIDC Flow (SSO)

When OpenID Connect providers are configured:

1. Frontend redirects user to the IDP's authorization URL.
2. User authenticates with the IDP.
3. IDP redirects back with an authorization `code`.
4. Console backend exchanges the code for MinIO STS credentials via `verifyUserAgainstIDP`.
5. Same cookie flow as above (steps 4–7).
6. An additional `idp-refresh-token` cookie is set for token refresh.

### Environment Variables

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `CONSOLE_PBKDF_PASSPHRASE` | Yes* | Random 64-char string | Passphrase for PBKDF2 key derivation |
| `CONSOLE_PBKDF_SALT` | Yes* | Random 64-char string | Salt for PBKDF2 key derivation |
| `CONSOLE_STS_DURATION` | No | `12h` | Session/STS token lifetime (Go duration format) |
| `CONSOLE_MINIO_SERVER` | Yes | — | BuckIt server endpoint URL |

\* If not set, random values are generated at startup — meaning sessions won't survive a restart.

### Session Validation

On each authenticated request:
1. Extract `token` cookie from the request.
2. Base64-decode and decrypt using the PBKDF2-derived key.
3. Unmarshal JSON to recover `STSAccessKeyID`, `STSSecretAccessKey`, `STSSessionToken`.
4. Use these credentials to create a MinIO/BuckIt client for the request.
5. If decryption fails or STS credentials are expired, return 401.

---

## mc CLI

The `mc` (MinIO Client) CLI authenticates directly against the BuckIt S3 API using **AWS Signature V4**. There is no session token or cookie mechanism — it uses long-term credentials stored locally.

### Flow Overview

```
┌──────────┐                              ┌──────────────┐
│  mc CLI  │                              │ BuckIt Server│
└────┬─────┘                              └──────┬───────┘
     │                                           │
     │  1. mc alias set mybuckit                 │
     │     http://localhost:9000                  │
     │     <accessKey> <secretKey>               │
     │  (stored in ~/.mc/config.json)            │
     │                                           │
     │  2. mc ls mybuckit/                       │
     │     GET / (signed with SigV4)             │
     │──────────────────────────────────────────>│
     │                                           │
     │  3. Server verifies SigV4 signature       │
     │<──────────────────────────────────────────│
     │     (response)                            │
```

### How It Works

1. **Alias configuration** — User registers a BuckIt endpoint with credentials:
   ```bash
   mc alias set mybuckit http://localhost:9000 ACCESS_KEY SECRET_KEY
   ```

2. **Credential storage** — Credentials are saved in `~/.mc/config.json`:
   ```json
   {
     "aliases": {
       "mybuckit": {
         "url": "http://localhost:9000",
         "accessKey": "ACCESS_KEY",
         "secretKey": "SECRET_KEY",
         "api": "S3v4",
         "path": "auto"
       }
     }
   }
   ```

3. **Request signing** — Every S3 API request is signed using AWS Signature V4 with the stored credentials. No intermediate token exchange.

4. **Admin operations** — For admin commands (`mc admin info`, `mc admin user`, etc.), mc uses the `madmin-go` library which also signs requests with SigV4 against the `/minio/admin/v3/` endpoints.

### Key Differences from Console

| Aspect | Console | mc CLI |
|--------|---------|--------|
| Auth mechanism | STS temporary credentials via cookie | Direct SigV4 with long-term credentials |
| Credential lifetime | Temporary (default 12h) | Permanent until rotated |
| Credential storage | Encrypted cookie (server-side encryption) | Plaintext in `~/.mc/config.json` |
| Token refresh | New login required when STS expires | Not needed — credentials don't expire |
| Multi-user | Per-session isolation via STS | Single credential per alias |

### Environment Variable Shortcut

mc also supports setting credentials via environment variables (useful in CI/CD):
```bash
export MC_HOST_mybuckit=http://ACCESS_KEY:SECRET_KEY@localhost:9000
mc ls mybuckit/
```

---

## Security Considerations

- **Console session tokens** are encrypted at rest (in the cookie) and can only be decrypted by the Console server that created them. Stealing the cookie without the PBKDF passphrase/salt is useless.
- **mc credentials** are stored in plaintext on disk. File permissions on `~/.mc/config.json` should be restricted (`chmod 600`).
- **STS credentials** (Console) have a bounded lifetime and limited blast radius if compromised. Long-term credentials (mc) require manual rotation.
- **LDAP users** cannot use `mc admin user` or `mc admin group` commands — user/group management is delegated to the LDAP directory.
