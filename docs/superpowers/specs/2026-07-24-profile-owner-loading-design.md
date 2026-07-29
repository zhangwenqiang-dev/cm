# Profile Owner Loading Design

## Problem

The server database contains the `iossupport-usw2` profile owner, and
`/api/profiles` already returns that owner. The web client subsequently calls
`loadProfileOwners()`, but clears `state.profileOwners` without making the
request when `clientConfig.user_api` is empty. `applyProfileOwners()` then
overwrites the valid owner returned by `/api/profiles`, so the UI displays `-`.

`user_api` selects the API routing mode. It must not decide whether profile
owners are available.

## Design

1. Treat each `/api/profiles` response as an authoritative owner snapshot.
   Build a fresh owner map from its embedded owners so a profile with an empty
   `owners` array clears any stale owner previously shown for that profile.
2. Always request `/api/profile-owners` through the existing `api()` helper.
   The helper already selects same-origin or configured remote routing.
3. Replace the owner map only after a successful owner response.
4. If the owner request fails, retain the owners already returned by
   `/api/profiles` instead of clearing them.
5. Continue applying the resulting owner map to all rendered profiles so the
   home table and operation detail show the same value.

## Error Handling

An owner endpoint failure must not fail the entire profile list load. The
profile list remains usable and displays exactly the owner data embedded in
the latest `/api/profiles` response, including clearing owners omitted by that
response. A later refresh can reconcile the owner map after the endpoint
recovers.

## Tests

Add static web regression coverage proving:

- `loadProfileOwners()` is not gated by `clientConfig.user_api`.
- A fresh map seeded from `/api/profiles` replaces the prior owner map, so
  embedded owners are shown and empty owner arrays clear stale entries.
- A failed `/api/profile-owners` request does not clear the seeded owners.
- A successful owner response still replaces the seeded map.

## Scope

This change does not alter owner assignment, release cleanup, AWS resources,
authentication, or database records.
