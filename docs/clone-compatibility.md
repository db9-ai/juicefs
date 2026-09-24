# FS9 clone metadata compatibility

FS9 restores raw TiKV metadata while retaining the source volume's object
identity. Source and target can therefore refer to the same objects. New writes
must receive distinct slice IDs, and copied locks must not become live owners in
the target. Existing version 1 volumes remain mountable without an offline
metadata migration.

## Allocation and locks

| Stored metadata version | New slice allocation | Lock layout |
| --- | --- | --- |
| 1, with or without a runtime allocator | Existing private `nextChunk` counter | Existing unscoped keys |
| 2 | Existing per-family external sequence | Existing physical-keyspace scope |
| 3 | Global ordinal with bit 63 set | Same physical-keyspace scope as version 2 |

Version 1 ignores `Config.SliceAllocator`, including an unavailable allocator.
Its format, allocation ranges, private counter and lock layout retain their
existing contract. Version 2 keeps its per-family sequence. Only a stored version
3 format enables global high-bit allocation. This separation assumes historical
signed legacy counters have never overflowed into the high-bit domain.

The deployment provisions `GlobalSliceAllocatorID` exactly once, initially at
ordinal 1, in a control keyspace excluded from all tenant snapshots and restores.
Each reservation commits a signed-positive ordinal range before returning it;
setting bit 63 yields the object ID. Ambiguous commits may waste ranges but must
never reuse them. A missing, corrupt, or exhausted configured allocator fails
allocation. Mount and clone preparation never create or reset its record. A lost
control keyspace requires explicit recovery above every previously reserved
ordinal, including abandoned reservations; initializing it again at 1 is unsafe.

Version 2 keeps its existing per-family allocation and lock contract. It is not
silently switched to the global sequence.

## Preparing an unpublished clone

`Meta.PrepareCloneFormat(ctx)` is called after restore and authority validation,
before the target has any session, mount, or lock owner. The caller owns this
unpublished-target precondition; the method is not a live-volume migration API.

Preparation verifies the global control record and physical metadata lock
identity, then changes only the stored format's `MetaVersion` and
`SliceAllocator` fields. Every other setting, including unknown fields, and all
counters, references, session rows, and lock rows are retained. Repeated
preparation is harmless. The client refreshes its cached format before returning,
so its first target fence uses the target's physical namespace. Replicas of that
target contend on the same lock keys; copied source locks remain untouched and
do not participate. Older clients reject version 3.

## Maintenance compatibility limits

Old mounts can read and write the legacy metadata layout, but this does **not**
make every old maintenance command safe. Older `juicefs gc` implementations parse
object IDs with signed `strconv.Atoi` and ignore overflow errors. They can treat
referenced high-bit objects as leaks and delete them with `--delete`. Such GC
binaries must not run against storage containing high-bit objects. The updated GC
uses unsigned parsing and leaves unparseable object IDs untouched.

Even the updated generic per-volume GC is not a clone-family collector: it sees
only one volume's references. A shared source/clone object namespace requires
family-wide retention and GC ownership; unsigned parsing alone does not make a
per-volume sweep safe. These operational restrictions must be enforced by the
FS9 deployment before enabling allocation or clones.

TiKV binary slice records and JSON dump/load preserve the full unsigned ID.
High-bit IDs do not advance the signed private counter when loading a JSON dump.
This FS9 compatibility contract covers TiKV metadata; it does not establish
high-bit support for SQL schemas or unrelated metadata drivers.

## Runtime upload and compaction boundaries

Legacy clients keep the existing PUT timeout, first-error Finish and immediate
Abort behavior. `chunk.Config.JoinUploads` is an explicit version 3 integration
option: it joins started physical PUTs even when providers ignore cancellation,
drains remaining upload results after an error, and serializes Finish with Abort.
Its use for retirement requires `Writeback=false`; staged background uploads do
not establish that physical completion boundary. Compaction already disables
writeback in the baseline implementation and continues to do so.

Only clients configured with `meta.Config.CompactionGuard` admit compactions
through a retirement guard, cancel them on close, and join them before releasing
session locks. FS9 configures this hook only for version 3. Legacy clients retain
background compaction contexts and the original session cleanup ordering.
`CompactContext` is explicitly cancellable; the existing `Compact` entry point
continues to use a background context.

TiKV reads and timestamp acquisition now honor their supplied context. This
corrects cancellation propagation without changing keys, counters or backend
dependencies, but canceled ordinary reads may return earlier than older builds.
The upload and compaction guarantees above concern orderly process shutdown.
They do not establish crash-safe GC: durable sid=0 lock ownership and recovery
remain an unresolved blocker.
