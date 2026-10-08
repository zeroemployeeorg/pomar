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
