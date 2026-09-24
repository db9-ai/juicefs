# FS9 family metadata protocols

FS9 restores raw TiKV metadata while retaining the source volume's object
identity. Source and target can therefore refer to the same objects. The stored
metadata version selects the family's protocol. Existing families and every
future descendant preserve that version; only a genuinely new, empty root
family selects version 2. Existing families retain their known allocation and
lifecycle limitations. Mounting or cloning them does not migrate their protocol.

## Allocation and locks

| Stored metadata version | New slice allocation | Lock layout |
| --- | --- | --- |
| 1, with or without a runtime allocator | Existing private `nextChunk` counter | Existing unscoped keys |
| 2 | Per-family external sequence, positive IDs in batches of 4096 | Physical-keyspace scope |

Version 1 ignores `Config.SliceAllocator`, including an unavailable allocator.
Version 2 stores an immutable family identity in `Format.SliceAllocator`.
Clones retain that identity and share its external sequence.
The deployment assigns each new root family a distinct physical object prefix,
so version 2 can start at ID 1 without colliding with existing families.

The deployment provisions each version 2 family sequence once in a control
keyspace excluded from tenant snapshots and restores. Each 4096-ID reservation
commits before returning it. Unused IDs from abandoned ranges and ambiguous
commits must never be reused. The sequence never wraps into bit 63. A missing,
corrupt, or exhausted configured allocator fails allocation; mounts and clones
never create or reset its record. A lost control keyspace requires explicit
recovery above every previously reserved ID, including abandoned reservations.

Physical lock scopes derive from the resolved TiKV cluster, keyspace and
metadata prefix, not the cloned format UUID. Replicas of a target contend on the
same keys. Restored source lock rows remain untouched and do not participate in
the target scope. Version 1 retains its original unscoped layout.

## Preparing an unpublished version 2 clone

The caller validates the restored format with `Meta.Load(true)`, requires
`MetaVersion == 2` and the expected `SliceAllocator`, and verifies the already
provisioned sequence with `SliceAllocator.Reserve(ctx, family, 0)`. The format is
immutable within the family. Preparation does not rewrite it, reserve IDs,
change counters or references, start a metadata session, or acquire a persistent
flock.

`Meta.CompareAndSwapXattr` atomically rebinds the target's authority attribute
from the exact restored source bytes to the target bytes. Nil expected bytes
require absence; a non-nil expectation requires an existing byte-for-byte match.
A mismatch returns `EAGAIN` without writing. The transaction changes only that
attribute, and replay handling belongs to the caller: after an ambiguous result,
read and validate the target authority. Semantically equal JSON is not an exact
byte match. Unsupported metadata drivers return `ENOTSUP`; TiKV requires a
nonempty replacement value. This API creates no durable lock owner that a
preparation crash could strand.

The caller owns the unpublished-target precondition and completes validation
and authority rebinding before exposing the target for mounts. This is not a
live-volume migration procedure.

Old clients that support only version 1 reject a version 2 format. Existing
version 1 families and restored descendants are never converted to version 2.

## Maintenance compatibility limits

Generic per-volume GC is not a clone-family collector: it sees only one
volume's references. Shared source/clone storage requires family-wide retention
and GC ownership, regardless of metadata version or ID range. The deployment
owns this restriction.

This contract covers TiKV metadata; it does not establish family protocol
support for SQL or unrelated metadata drivers.

## Runtime upload and compaction boundaries

JuiceFS keeps lifecycle changes opt-in. `chunk.Config.JoinUploads` joins started
physical PUTs even when providers ignore cancellation, drains remaining results
after an error, and serializes Finish with Abort. That physical completion
boundary requires `Writeback=false`; staged background uploads do not establish
it. Compaction already disables writeback. These orderly shutdown guarantees
are separate from irreversible version 2 retirement.

`meta.Config.CompactionGuard` admits compactions through a retirement guard,
cancels them on close, and joins them before releasing session locks. FS9 enables
both lifecycle options only for version 2 families. Version 1 families retain
the existing PUT timeout, first-error Finish, immediate Abort, background
compaction contexts and session cleanup ordering. `CompactContext` is explicitly
cancellable; the existing `Compact` uses a background context.

TiKV reads and timestamp acquisition honor their supplied context. This changes
no keys, counters or backend dependencies, but canceled ordinary reads may
return earlier than older builds.

Version 2 retirement advances ordinary root authority from NORMAL to GC_PENDING,
then to GC_COMPLETE, using exact-byte xattr CAS. It does not acquire a persistent
Flock, so a crashed sid=0 holder cannot block that transition. The backend must
first authorize irreversible retirement and exclude conflicting lifecycle work;
final family GC additionally requires every member and restore dependency to
have retired. FS9 rejects pending mutations, migration bindings, receipts,
quarantine and unknown authority fields. Version 1 retains its previous
retirement path and limitations.

GC_PENDING rejects new foreground and compaction admissions. An already admitted
writer may still finish against dead member metadata; its ordinary completion
cannot restore NORMAL. This neither repairs stale locks on an active volume nor
promises general lock recovery. A timeout, TTL or CAS receipt is not evidence
that an admitted writer or provider PUT has physically stopped.

GC_COMPLETE records a collection pass that observed an empty chunk prefix with
no reported failures. Young objects and incomplete passes remain pending. An
extremely late provider PUT can leave an orphan after that observation; no
permanent or recurring sweep is promised. This protocol description is not
passing evidence for the separate real TiKV SIGKILL qualification gate.
