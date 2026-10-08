# Target resource coverage and platform limits

A process's unavailable executable path is distinct from custody of a retained
workspace. The probe attempts full kernel birth/UID and target inode coverage
for a foreign process whose executable path is missing. Complete descriptor,
fileport and mapped-region enumeration, bounded inventory, stable full birth
including microseconds and an unchanged target inode can exclude that process
for this target. A name, UID, old PID's absence or empty failed syscall cannot.
The preboot absent-path case cannot exclude unknown unlinked inodes this way.

When full foreign identity is denied, the probe reports availability at all
three resource boundaries separately. These probes never count as completed
coverage. EPERM on identity, FD inventory, fileport inventory or regions leaves
custody unknown. A missing executable and another owner's known process name
are not a basis for deleting a workspace or stopping that other process.

On macOS 27, an ordinary development owner can receive a short kernel UID while
full birth and all three resource APIs return EPERM for foreign accounts whose
executable lookup returns ENOENT. This observer cannot retrospectively exclude
those processes from a retained disk. No negative target-specific fallback is
available from those observations alone. Keep deletion refused. A differently
privileged resource observer would be a separately reviewed, scoped operator
arrangement; broad sudo, process termination or permission relaxation is not a
fallback provided by this implementation.

Launch provenance narrows future investigation but is not retirement clearance.
Preserve the target inode and helper's full kernel identity before virtualization
creation, joined to environment/session/incarnation/source and the observed executable file inode/digest. That file digest is not a
retrospective measurement of running executable mappings.
It cannot by itself identify a privileged Virtualization service's inherited or
received fileports, exclude later descriptor transfer, or replace a current
resource fence. Legacy environments have no retrospectively synthesized launch
receipt. Never clear them merely because a newer launcher records more facts.


Coverage failures, including a process vanishing during resource enumeration,
remain UNKNOWN for that observation. A complete scan must retain stable full
kernel identity around all resource inventories. Negative sizes are failures
regardless of errno; no missing-path issue from an earlier pass/identity is
removed by a later PID observation. Launch receipts fully flush both file and
directory on macOS (`F_FULLFSYNC`) before VM creation. Failure retains the
receipt and blocks creation/replay; it never certifies a legacy launch.

`pomar-host agent-environment-holder-diagnostic --config ROOT_PRIVATE_FILE`
is a separate one-shot administrator observation. It requires actual real and
effective UID0 and a root-owned single-link0600 bounded config. The config binds
one existing target inode/device plus the owner's original environment/session/
incarnation/operation/helper identity. The scan reads native metadata, not file
contents, argv, process environments or provider credentials. Its bounded result
contains target binding, coverage counts and PID/category locators, without
unrelated executable/vnode paths. A PID locator must be freshly identified by
its owner before any action. The result is explicitly diagnostic-only and is
not accepted as a fence, retirement, actor admission or process-control request.
An administrator runs a reviewed, independently hash-pinned staged binary/config;
no service, permanent delegation or permissions change is required. Ordinary
UID503 tests verify refusal without elevation; the actual privileged diagnostic
remains a separately attended observation, not a claimed unit-test result.
