# Security design: local database keys

Each installation of the desktop app keeps the clinic's data in a local SQLCipher
database. Its key never stays on disk in a usable form: it comes from this
server at login and is rotated on every online login. Patient records (not
financial data) are also synced to this server; see "Patient records" below.

The API contract is in `proto/dental/v1/` — the comments there are the reference
for message formats and the crypto scheme.

## Pieces

| Where | What |
|---|---|
| Server (PostgreSQL) | users (+ identity: national ID or FIDA code, and nationality), clinics (+ unique license number, owner), installations (+ machine info, clinic), clinic membership + admin flag, subscriptions of clinic owners, database keys (encrypted with `KEY_ENCRYPTION_KEY`), sessions; administrators and their sessions, kept apart from users; patients and their treatments (never financial data), and the log of who viewed a patient's history |
| Desktop app (Rust) | installation id + secret (DPAPI), per-user offline vault, roles and permissions (inside the encrypted local DB) |
| Both | server Ed25519 public key built into the app, to verify offline grants |

## First start

1. App generates nothing secret yet; it collects `MachineInfo` (MachineGuid hash, hostname, OS, CPU, RAM, MAC addresses).
2. The user fills in either a new clinic (name, unique license number, phone, address,
   map location) and its owner — a new account (with a unique identity: national ID, or FIDA
   code for foreign nationals) or an existing one (username + password; the identity only if
   none is on file) — or the license number of an existing clinic and its owner's
   credentials, to add another installation to it. Alternatively the person logs in (or signs
   up) first: `InstallationService.ListMyClinics`, proven with the password like the existing
   owner, lists the clinics they own or administer, and they pick the clinic this computer
   belongs to (only its owner may) or add a new one. The app keeps the password in memory
   only until the first login.
3. `InstallationService.Register` → `installation_id` + `installation_secret`. The owner is
   already a member and admin of the clinic.
4. App stores both, the secret encrypted with DPAPI.

Registering needs no subscription, but logging in does (below).

## Accounts, clinics and subscriptions

- An account belongs to a person, unique by identity across the whole system. A clinic admin
  can create one for someone with a temporary password (`MemberService.CreateUser`; if the
  person already has an account, that one is returned). The temporary password must be
  changed with `AuthService.ChangePassword` before the first login.
- Membership is per clinic: a member may log in on every installation of the clinic. Roles
  (doctor, secretary...) stay in the local database.
- Only the clinic owner buys a subscription (from the admin panel for now). It limits how many
  clinics the owner may have and how many installations (not revoked) each clinic may have;
  when there are more, the oldest ones are covered.
- Without a subscription in force, nobody can use the owner's clinics: logins fail, sessions
  stop working at once, and offline grants never last past the end of the paid time.
- Administrators (`AdminService`) have their own accounts and sessions. A user session never
  works on the admin panel and an admin session never works anywhere else. The first
  administrator is created on the server with `dental-server create-admin`.
- An admin session is proven by two things on every call: a JWT (HS256, signed with
  `ADMIN_TOKEN_KEY`) in the `dental_admin_token` cookie — HttpOnly, Secure, SameSite=Strict,
  sent only to `AdminService` paths, so scripts in the page cannot steal it — and the session's
  CSRF token in the `X-CSRF-Token` header, which another site cannot know (stored only as a
  SHA-256 hash). The JWT names its `admin_sessions` row, which is checked on every call, so
  logging out or disabling the administrator ends the session at once, before the JWT expires.

## Online login

1. App makes a one-time X25519 key pair and calls `AuthService.Login` with
   installation id + secret, username + password and the public key.
2. Server checks the secret, the password, membership of the installation's clinic and the
   clinic owner's subscription (in force, and covering this clinic and installation).
3. Server returns a session token, the `DatabaseKeys` sealed to the one-time key,
   and a signed `OfflineGrant` (expiry = now + `installations.offline_access`, but never
   later than the end of the owner's paid time).
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

Login without the server: check the database file looks usable without its key
(present, whole pages, encrypted), unlock the private key with the password (a
wrong password fails decryption), verify the grant signature, check it has not
expired, open the database.

The app is "online" only when `HealthService.Check` answers with the database
ready; network access alone is not enough. One watcher in the app checks it
periodically and whenever Windows reports a network change.

### Reconnecting

An offline session keeps the user's password in memory (wiped on drop) so it can
log in online by itself when the server is back. That login rekeys the
already-open database in place, refreshes the vault and confirms the rotation,
exactly like a normal online login, and the user keeps working. If the server
now refuses the user (`not_a_member`, `user_disabled`, `installation_revoked`,
`subscription_inactive`, `clinic_limit_exceeded`, `device_limit_exceeded`, `no_clinic`),
the session ends at once and offline access is removed.

After every online login the app also calls `MemberService.ListMembers` and
removes the vault entries of users who are no longer members, so access removed
on the server stops working offline too, without waiting for those users to try.

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

## Patient records

The desktop app always works on its local database, online or offline. Patients
and their treatments are also synced with this server (`SyncService`,
`proto/dental/v1/sync.proto`):

- Every synced row carries a `sync_state` in the local database: `new` (never stored
  by the server), `changed` (waiting to be sent), `synced`, or `rejected` (refused by
  the server; not sent again until it is fixed). Changes made offline wait there and
  are pushed as soon as the session is online.
- **Financial data never leaves the computer.** The API has no field for it, the app
  only reads the tables listed in `SYNCED_TABLES` (src-tauri/src/localdb.rs), and a
  test there fails when a table is added without being declared synced or local-only,
  or when a synced table has a column that looks financial.
- A patient is a person, unique across the system by identity (`PersonIdentity`). The
  patient id is derived from the identity (SHA-256, see `Patient.id`), so installations
  that add the same person offline agree on the id. Every clinic that sends a patient,
  or records a treatment for them, is linked to them (`clinic_patients`) and receives
  later changes to their details.
- Treatments belong to the clinic that recorded them: only its installations receive
  or change them. Deletions are kept as tombstones, so they sync too.
- The latest change wins, record by record (`updated_at`, the client's clock, capped at
  the server's time so a clock running ahead cannot win forever).
- Pull returns changes after a revision (`sync_revision_seq`). Every synced write takes
  its revision inside one global advisory lock, so revisions become visible in order
  and an installation never skips a change.
- Any member of any clinic can see a patient's history across clinics by identity
  (`PatientService.GetPatientHistory`). Every view is logged in
  `patient_history_views` (who, which clinic and installation, when), in the same
  transaction as the read. Patient consent (an SMS code) is planned before this opens up.

## Forced updates

The server publishes the current desktop app release from `UPDATES_DIR`
(`internal/releases`): the installer, signed with the release signing key (its
public half is built into the app), and `release.json`. Apps update themselves from
it. Its `minimumVersion` forces an update: `HealthService.Check` reports it, older
apps stop working until they have updated (they remember it, so this holds offline
too), and `AppVersionInterceptor` refuses their calls with `update_required`
(apps send their version in the `Dental-App-Version` header).
