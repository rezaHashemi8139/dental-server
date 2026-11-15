# Security design: local database keys

Each installation of the desktop app keeps patient data in a local SQLCipher
database. Its key never stays on disk in a usable form: it comes from this
server at login and is rotated on every online login.

The API contract is in `proto/dental/v1/` — the comments there are the reference
for message formats and the crypto scheme.

## Pieces

| Where | What |
|---|---|
| Server (PostgreSQL) | users, installations (+ machine info), membership + admin flag, database keys (encrypted with `KEY_ENCRYPTION_KEY`), sessions |
| Desktop app (Rust) | installation id + secret (DPAPI), per-user offline vault, roles and permissions (inside the encrypted local DB) |
| Both | server Ed25519 public key built into the app, to verify offline grants |

## First start

1. App generates nothing secret yet; it collects `MachineInfo` (MachineGuid hash, hostname, OS, CPU, RAM, MAC addresses).
2. `InstallationService.Register` → `installation_id` + `installation_secret`.
3. App stores both, the secret encrypted with DPAPI.

## Online login

1. App makes a one-time X25519 key pair and calls `AuthService.Login` with
   installation id + secret, username + password and the public key.
2. Server checks the secret, the password and membership (the first user to
   log in on an installation becomes owner + admin).
3. Server returns a session token, the `DatabaseKeys` sealed to the one-time key,
   and a signed `OfflineGrant` (expiry = now + `installations.offline_access`).
4. App opens the database (current key, or next key if a previous rotation was not
   confirmed), rekeys to the next key, verifies, then `ConfirmKeyRotation`.
5. App updates the offline vault (below) with the new key for every user in it.

## Offline login

Per user who has logged in online on this installation, the app keeps:

- an X25519 key pair; the private key is encrypted with a key derived from the
  user's password (Argon2id), then wrapped with DPAPI;
- the current database key sealed to that user's public key;
- the latest signed `OfflineGrant`.

Because database keys are sealed to *public* keys, a rotation by one user updates
every other user's copy without knowing their passwords.

Login without the server: unlock the private key with the password (a wrong
password fails decryption), verify the grant signature, check it has not
expired, open the database.

### Clock tampering

- `issued_at` / `expires_at` are server times, signed.
- The app keeps a "latest time seen" in the vault and in the registry, and
  refuses offline login if the clock is earlier than either, or earlier than the
  newest modification time of some Windows system files.

## Logout

The database is closed and the key is wiped from memory. The next person needs
their own credentials.

## Blocking a stolen computer

Set `installations.revoked_at`: no more logins or keys from the server. Offline
logins keep working until their grant expires, so keep `offline_access` short.
