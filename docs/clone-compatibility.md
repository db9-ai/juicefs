# FS9 family metadata protocols

FS9 restores raw TiKV metadata while retaining the source volume's object
identity. Source and target can therefore refer to the same objects. The stored
metadata version selects the family's protocol. Existing families and every
future descendant preserve that version; only a genuinely new, empty root
family selects version 4. Existing families retain their known allocation and
lifecycle limitations. Mounting or cloning them does not migrate their protocol.

## Allocation and locks

| Stored metadata version | New slice allocation | Lock layout |
| --- | --- | --- |
| 1, with or without a runtime allocator | Existing private `nextChunk` counter | Existing unscoped keys |
| 2 | Existing per-family external sequence | Existing physical-keyspace scope |
| 3 | Existing global ordinal with bit 63 set | Existing physical-keyspace scope |
| 4 | Per-family external sequence, positive IDs in batches of 4096 | Physical-keyspace scope |

Version 1 ignores `Config.SliceAllocator`, including an unavailable allocator.
Versions 2 and 3 keep their allocation and lock contracts. Version 4 uses the
same allocator range semantics as version 2, with an immutable family identity
in `Format.SliceAllocator`. Clones retain that identity and share its sequence.
The deployment assigns each new root family a distinct physical object prefix,
so version 4 can start at ID 1 without colliding with existing families. It does
not depend on a high-bit separation assumption about historical IDs.

The deployment provisions each version 4 family sequence once in a control
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

## Preparing an unpublished version 4 clone

The caller validates the restored format with `Meta.Load(true)`, requires
`MetaVersion == 4` and the expected `SliceAllocator`, and verifies the already
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

The historical `Meta.PrepareCloneFormat(ctx)` API remains available for its
original unpublished v1/v2-to-v3 migration contract. It preserves unrelated
format fields, including unknown settings, but is not called by the family
version rollout. It explicitly rejects version 4. Old clients that support only
versions 1–3 reject a version 4 format.

## Maintenance compatibility limits

Version 3 still uses `GlobalSliceAllocatorID` and high-bit object IDs. Its
historical separation from legacy counters assumes those counters never
entered the high-bit domain. Older `juicefs gc` implementations parse IDs with
signed `strconv.Atoi` and ignore overflow errors; they can mistake referenced
high-bit objects for leaks. They must not run against that storage. The updated
GC uses unsigned parsing and leaves unparseable IDs untouched.

Generic per-volume GC is not a clone-family collector: it sees only one
volume's references. Shared source/clone storage requires family-wide retention
and GC ownership, regardless of metadata version or ID range. The deployment
owns this restriction.

TiKV binary slice records and JSON dump/load preserve unsigned IDs. High-bit
IDs do not advance the signed private counter on JSON load. This contract covers
TiKV metadata; it does not establish high-bit support for SQL or other drivers.

## Runtime upload and compaction boundaries

JuiceFS keeps lifecycle changes opt-in. `chunk.Config.JoinUploads` joins started
physical PUTs even when providers ignore cancellation, drains remaining results
after an error, and serializes Finish with Abort. Retirement requires
`Writeback=false`; staged background uploads do not establish that physical
completion boundary. Compaction already disables writeback.

`meta.Config.CompactionGuard` admits compactions through a retirement guard,
cancels them on close, and joins them before releasing session locks. FS9 enables
both lifecycle options for new version 4 families and retains them for the
historical version 3 draft protocol. Production version 1 and 2 families retain
the existing PUT timeout, first-error Finish, immediate Abort, background
compaction contexts and session cleanup ordering. `CompactContext` is explicitly
cancellable; the existing `Compact` uses a background context.

TiKV reads and timestamp acquisition honor their supplied context. This changes
no keys, counters or backend dependencies, but canceled ordinary reads may
return earlier than older builds.

These lifecycle guarantees concern orderly shutdown. They do not establish
crash-safe GC: durable sid=0 lock ownership and recovery remain an unresolved
blocker. A timeout or TTL is not evidence that an admitted writer has stopped.
