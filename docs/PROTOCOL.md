# Deposit protocol v2

The current endpoint is `POST /v2/backups`, with no query string.
`POST /v1/backups` remains available for older clients, which do not provide
idempotent retries. HTTPS, TLS 1.3, a non-empty file, and a known content length
are required. There are no public administration, read, list, status, recovery,
or delete endpoints.

## Request

| Header | Content |
|---|---|
| `Authorization` | `Bearer <key>`; at most 255 printable ASCII characters with no spaces. |
| `Content-Type` | `application/octet-stream` |
| `Content-Length` | Positive content size in bytes. |
| `X-Onlybackup-Metadata` | UTF-8 JSON encoded as unpadded base64url. |
| `X-Onlybackup-Sha256` | SHA-256 of the content as 64 lowercase hexadecimal characters. |
| `X-Onlybackup-Idempotency-Key` | Required in v2: a random operation key containing 16 to 128 alphanumeric, `-`, or `_` characters. |
| `Expect` | The official client uses `100-continue` so the server can reject a request before receiving its body. |

The metadata JSON requires `description` and `original_name`. It may include
`content_format`, omitted or empty for plaintext, or set to `age-v1` for content
encrypted by the client with age. Other values are rejected. The credential is
sent in the authorization header. The client calculates the size and checksum
from the bytes that will be stored.

```json
{"description":"Accounting backup","original_name":"accounting.sql.gz"}
```

`description` is required and limited to 1,000 Unicode code points.
`original_name` is required and limited to 255 characters. It cannot be `.` or
`..`, contain a slash or backslash, or contain control characters. File
extensions are not filtered. The client uses the local filename when no name is
provided. Metadata cannot contain additional properties or concatenated JSON
values.

The body contains raw bytes, with no multipart framing, extraction, conversion,
or decompression. `Content-Encoding` is not accepted. The original filename is
stored only in the database.

Data streams through the receiver without a disk copy or archive access. The
writer authenticates the request, applies catalog quotas and limits, reserves
space, and calculates the hash. Admission decisions and replay of a completed
deposit occur before the writer reads the body. Responses never expose the
physical path, credential, or backup content.

If a rejection, completed replay, or final error occurs before the request body
has finished, the writer and receiver stop the pending read before returning
the response. The affected HTTP/1.1 connection is not reused. This also lets a
client that has sent only headers receive an immediate response instead of
waiting for the upload timeout. An admitted request that is interrupted releases
reserved space and concurrency, but its counted attempt remains.

With encryption enabled, the body is the age file rather than the original
plaintext. Quotas, length, and hash cover that stored body. The declared format
does not prove that the content can be decrypted because the writer has no
private key. The name and description remain plaintext catalog metadata. Only
recovery with the age identity can authenticate and return decrypted content.

## Response

The server returns `201 Created` only after storage and catalog confirmation:

```json
{
  "id": "0123456789abcdef0123456789abcdef",
  "status": "complete",
  "size_bytes": 12345,
  "sha256": "...64 lowercase hexadecimal characters...",
  "received_at": "2026-09-10T15:00:00Z"
}
```

The server assigns a random 128-bit ID and records its UTC clock time. SHA-256
checks integrity; it does not sign the receipt. TLS authenticates the connection.
Independent or offline proof of receipt origin would require a separate signing
mechanism, which OnlyBackup does not currently provide.

Errors use the form `{"error":"message"}`:

| HTTP | Meaning |
|---|---|
| 400 | Invalid or incomplete metadata, headers, or transfer. |
| 401 | Unknown or revoked credential. |
| 404 / 405 | Path or method not available. |
| 409 | The idempotency key is in use for different data or an upload still in progress. |
| 411 | Unknown content length or empty file. |
| 413 | File exceeds the profile limit. |
| 415 | Unsupported body type or encoding. |
| 422 | Content does not match the supplied SHA-256 digest. |
| 429 | Key quota, rate, or concurrency limit exceeded. |
| 502 / 503 | Writer unavailable, server busy, or outcome not confirmed. |
| 507 | Insufficient disk space or storage operation unavailable. |

A disconnection or error before receipt delivery does not prove that the backup
is absent. The official client creates a random idempotency key for each
operation and retries the same upload at most twice after a network error or
temporary unavailable response. The writer binds the key to the credential,
metadata, size, and SHA-256 digest. If the deposit is already complete, it
returns the original receipt without creating another backup. Reusing the key
with different data is rejected.

Older clients using v1 may omit the idempotency header and retain the original
behavior: each successful request creates a separate deposit.

The `uploads_per_day` limit counts each admitted transfer in the rolling
24-hour window, including transfers that are interrupted, fail, or retry an
idempotency key from a failed attempt. Rejections before admission do not consume
an attempt. Replaying a completed deposit returns the original receipt without
a transfer, space reservation, or new attempt. Attempt rows outside the window
are removed on the next admission for the same key.

## Logical transaction

1. Validate and reserve atomically in the database: create the `receiving`
   state and record the attempt.
2. Exclusively create `incoming/<ID>.part`; copy and verify the content length.
3. Verify SHA-256, apply mode 0400, synchronize, and close the file.
4. Hard-link to `backups/<ID>.backup` without replacement and synchronize the
   directory.
5. Remove the temporary file and synchronize the incoming directory.
6. Set the database state to `complete` and return 201.

The filesystem and SQLite do not form a single transaction. If the process
stops between publication and database confirmation, startup reconciliation
verifies the file and completes the record. If the final file does not exist,
the record becomes `failed` and the temporary file is removed. Reconciliation
never deletes final backup files.
