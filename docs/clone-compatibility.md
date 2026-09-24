# FS9 clone metadata compatibility

FS9 restores raw TiKV metadata while retaining the source volume's object
identity. Source and target can therefore refer to the same objects. New writes
must receive distinct slice IDs, and copied locks must not become live owners in
the target. Existing version 1 volumes remain mountable without an offline
metadata migration.

## Allocation and locks

| Stored metadata version | New slice allocation | Lock layout |
| --- | --- | --- |
| 1, no runtime allocator | Existing private `nextChunk` counter | Existing unscoped keys |
| 1, runtime allocator configured | Global ordinal with bit 63 set | Existing unscoped keys |
| 2 | Existing per-family external sequence | Existing physical-keyspace scope |
| 3 | Global ordinal with bit 63 set | Same physical-keyspace scope as version 2 |

Configuring `Config.SliceAllocator` on a legacy volume changes only new
allocations. It does not rewrite the stored format, advance its private counter,
change its lock layout, or invalidate ranges cached by older writers. Legacy
writers can continue allocating low IDs while updated writers allocate high IDs.
This separation assumes that historical signed legacy counters have never
overflowed into the high-bit domain.

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
