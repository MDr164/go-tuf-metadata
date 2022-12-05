package trustedmetadata

import (
	"fmt"
	"time"

	"github.com/rdimitrov/ngo-tuf/metadata"
)

// TrustedMetadata struct for storing trusted metadata
type TrustedMetadata struct {
	root      *metadata.Metadata[metadata.RootType]
	snapshot  *metadata.Metadata[metadata.SnapshotType]
	timestamp *metadata.Metadata[metadata.TimestampType]
	targets   map[string]*metadata.Metadata[metadata.TargetsType]
	refTime   time.Time
}

// New creates a new TrustedMetadata instance which ensures that the
// collection of metadata in it is valid and trusted through the whole
// client update workflow. It provides easy ways to update the metadata
// with the caller making decisions on what is updated.
func New(rootData []byte) (*TrustedMetadata, error) {
	res := &TrustedMetadata{
		targets: map[string]*metadata.Metadata[metadata.TargetsType]{},
		refTime: time.Now().UTC(),
	}
	// load and validate the local root metadata.
	// Valid initial trusted root metadata is required
	err := res.loadTrustedRoot(rootData)
	if err != nil {
		return nil, err
	}
	return res, nil
}

// loadTrustedRoot verifies and loads "data" as trusted root metadata.
// Note that an expired initial root is considered valid: expiry is
// only checked for the final root in “UpdateTimestamp()“.
func (trusted *TrustedMetadata) loadTrustedRoot(rootData []byte) error {
	// generate root metadata
	newRoot, err := metadata.Root().FromBytes(rootData)
	if err != nil {
		return err
	}
	// check metadata type matches root
	if newRoot.Signed.Type != metadata.ROOT {
		return fmt.Errorf("expected %s, got %s", metadata.ROOT, newRoot.Signed.Type)
	}
	// verify root by itself
	err = newRoot.VerifyDelegate(metadata.ROOT, newRoot)
	if err != nil {
		return err
	}
	// save root if verified
	trusted.root = newRoot
	fmt.Println("Loaded trusted root v", trusted.root.Signed.Version)
	return nil
}

// UpdateRoot verifies and loads “data“ as new root metadata.
// Note that an expired intermediate root is considered valid: expiry is
// only checked for the final root in “UpdateTimestamp()“.
func (trusted *TrustedMetadata) UpdateRoot(rootData []byte) (*metadata.Metadata[metadata.RootType], error) {
	if trusted.timestamp != nil {
		return nil, fmt.Errorf("cannot update root after timestamp")
	}
	fmt.Println("Updating root")
	// generate root metadata
	newRoot, err := metadata.Root().FromBytes(rootData)
	if err != nil {
		return nil, err
	}
	// check metadata type matches root
	if newRoot.Signed.Type != metadata.ROOT {
		return nil, fmt.Errorf("expected %s, got %s", metadata.ROOT, newRoot.Signed.Type)
	}
	// verify that new root is signed by trusted root
	err = trusted.root.VerifyDelegate(metadata.ROOT, newRoot)
	if err != nil {
		return nil, err
	}
	// verify version
	if newRoot.Signed.Version != trusted.root.Signed.Version+1 {
		return nil, fmt.Errorf("bad version number, expected %d, got %d", trusted.root.Signed.Version+1, newRoot.Signed.Version)
	}
	// verify that new root is signed by itself
	err = newRoot.VerifyDelegate(metadata.ROOT, newRoot)
	if err != nil {
		return nil, err
	}
	// save root if verified
	trusted.root = newRoot
	fmt.Printf("Updated root v%d\n", trusted.root.Signed.Version)
	return trusted.root, nil
}

// UpdateTimestamp verifies and loads “data“ as new timestamp metadata.
// Note that an intermediate timestamp is allowed to be expired. "TrustedMetadata"
// will error in this case but the intermediate timestamp will be loaded.
// This way	a newer timestamp can still be loaded (and the intermediate
// timestamp will be used for rollback protection). Expired timestamp will
// prevent loading snapshot metadata.
func (trusted *TrustedMetadata) UpdateTimestamp(timestampData []byte) (*metadata.Metadata[metadata.TimestampType], error) {
	if trusted.snapshot != nil {
		return nil, fmt.Errorf("cannot update timestamp after snapshot")
	}
	// client workflow 5.3.10: Make sure final root is not expired.
	if trusted.root.Signed.IsExpired(trusted.refTime) {
		return nil, fmt.Errorf("final root.json is expired")
	}
	fmt.Println("Updating timestamp")
	// no need to check for 5.3.11 (fast forward attack recovery):
	// timestamp/snapshot can not yet be loaded at this point
	newTimestamp, err := metadata.Timestamp().FromBytes(timestampData)
	if err != nil {
		return nil, err
	}
	// check metadata type matches timestamp
	if newTimestamp.Signed.Type != metadata.TIMESTAMP {
		return nil, fmt.Errorf("expected %s, got %s", metadata.TIMESTAMP, newTimestamp.Signed.Type)
	}
	// verify that new timestamp is signed by trusted root
	err = trusted.root.VerifyDelegate(metadata.TIMESTAMP, newTimestamp)
	if err != nil {
		return nil, err
	}
	// if an existing trusted timestamp is updated,
	// check for a rollback attack
	if trusted.timestamp != nil {
		// prevent rolling back timestamp version
		if newTimestamp.Signed.Version < trusted.timestamp.Signed.Version {
			return nil, fmt.Errorf("new timestamp version %d must be >= %d", newTimestamp.Signed.Version, trusted.timestamp.Signed.Version)
		}
		// keep using old timestamp if versions are equal.
		if newTimestamp.Signed.Version == trusted.timestamp.Signed.Version {
			return nil, fmt.Errorf("new timestamp version %d equals the old one %d", newTimestamp.Signed.Version, trusted.timestamp.Signed.Version)
		}
		// prevent rolling back snapshot version
		snapshotMeta := trusted.timestamp.Signed.Meta[fmt.Sprintf("%s.json", metadata.SNAPSHOT)]
		newSnapshotMeta := newTimestamp.Signed.Meta[fmt.Sprintf("%s.json", metadata.SNAPSHOT)]
		if newSnapshotMeta.Version < snapshotMeta.Version {
			return nil, fmt.Errorf("new snapshot version %d must be >= %d", newSnapshotMeta.Version, snapshotMeta.Version)
		}
	}
	// expiry not checked to allow old timestamp to be used for rollback
	// protection of new timestamp: expiry is checked in UpdateSnapshot()
	// save root if verified
	trusted.timestamp = newTimestamp
	fmt.Printf("Updated timestamp v%d\n", trusted.timestamp.Signed.Version)

	// timestamp is loaded: error if it is not valid _final_ timestamp
	err = trusted.checkFinalTimestamp()
	if err != nil {
		// return the new timestamp but also the error if it's expired
		return trusted.timestamp, err
	}
	return trusted.timestamp, nil
}

// checkFinalTimestamp verifies if trusted timestamp is not expired
func (trusted *TrustedMetadata) checkFinalTimestamp() error {
	if trusted.timestamp.Signed.IsExpired(trusted.refTime) {
		return fmt.Errorf("timestamp.json is expired")
	}
	return nil
}

// UpdateSnapshot verifies and loads “data“ as new snapshot metadata.
// Note that an intermediate snapshot is allowed to be expired and version
// is allowed to not match timestamp meta version: TrustedMetadata
// will error for case of expired metadata or when using bad versions but the
// intermediate snapshot will be loaded. This way a newer snapshot can still
// be loaded (and the intermediate snapshot will be used for rollback protection).
// Expired snapshot or snapshot that does not match timestamp meta version will
// prevent loading targets.
func (trusted *TrustedMetadata) UpdateSnapshot(snapshotData []byte, isTrusted bool) (*metadata.Metadata[metadata.SnapshotType], error) {
	if trusted.timestamp == nil {
		return nil, fmt.Errorf("cannot update snapshot before timestamp")
	}
	if trusted.targets[metadata.TARGETS] != nil {
		return nil, fmt.Errorf("cannot update snapshot after targets")
	}
	fmt.Println("Updating targets")

	// snapshot cannot be loaded if final timestamp is expired
	err := trusted.checkFinalTimestamp()
	if err != nil {
		return nil, err
	}
	snapshotMeta := trusted.timestamp.Signed.Meta[fmt.Sprintf("%s.json", metadata.SNAPSHOT)]
	// verify non-trusted data against the hashes in timestamp, if any.
	// trusted snapshot data has already been verified once.
	if !isTrusted {
		err = snapshotMeta.VerifyLengthHashes(snapshotData)
		if err != nil {
			return nil, err
		}
	}
	newSnapshot, err := metadata.Snapshot().FromBytes(snapshotData)
	if err != nil {
		return nil, err
	}
	// check metadata type matches snapshot
	if newSnapshot.Signed.Type != metadata.SNAPSHOT {
		return nil, fmt.Errorf("expected %s, got %s", metadata.SNAPSHOT, newSnapshot.Signed.Type)
	}
	// verify that new snapshot is signed by trusted root
	err = trusted.root.VerifyDelegate(metadata.SNAPSHOT, newSnapshot)
	if err != nil {
		return nil, err
	}

	// version not checked against meta version to allow old snapshot to be
	// used in rollback protection: it is checked when targets is updated

	// if an existing trusted snapshot is updated, check for rollback attack
	if trusted.snapshot != nil {
		for name, info := range trusted.snapshot.Signed.Meta {
			newFileInfo, ok := newSnapshot.Signed.Meta[name]
			// prevent removal of any metadata in meta
			if !ok {
				return nil, fmt.Errorf("new snapshot is missing info for %s", name)
			}
			// prevent rollback of any metadata versions
			if newFileInfo.Version < info.Version {
				return nil, fmt.Errorf("expected %s version %d, got %d", name, newFileInfo.Version, info.Version)
			}
		}
	}

	// expiry not checked to allow old snapshot to be used for rollback
	// protection of new snapshot: it is checked when targets is updated
	trusted.snapshot = newSnapshot
	fmt.Printf("Updated snapshot v%d\n", trusted.snapshot.Signed.Version)

	// snapshot is loaded, but we error if it's not valid _final_ snapshot
	err = trusted.checkFinalSnapshot()
	if err != nil {
		// return the new snapshot but also the error if it's expired
		return trusted.snapshot, err
	}
	return trusted.snapshot, nil
}

// checkFinalSnapshot verifies if it's not expired and snapshot version matches timestamp meta version
func (trusted *TrustedMetadata) checkFinalSnapshot() error {
	if trusted.snapshot.Signed.IsExpired(trusted.refTime) {
		return fmt.Errorf("snapshot.json is expired")
	}
	snapshotMeta := trusted.timestamp.Signed.Meta[fmt.Sprintf("%s.json", metadata.SNAPSHOT)]
	if trusted.snapshot.Signed.Version != snapshotMeta.Version {
		return fmt.Errorf("expected %d, got %d", snapshotMeta.Version, trusted.snapshot.Signed.Version)
	}
	return nil
}

// UpdateTargets verifies and loads “data“ as new top-level targets metadata.
func (trusted *TrustedMetadata) UpdateTargets(targetsData []byte) (*metadata.Metadata[metadata.TargetsType], error) {
	return trusted.updateDelegatedTargets(targetsData, metadata.TARGETS, metadata.ROOT)
}

// updateDelegatedTargets verifies and loads “data“ as new metadata for target “role_name“
func (trusted *TrustedMetadata) updateDelegatedTargets(targetsData []byte, roleName, delegatorName string) (*metadata.Metadata[metadata.TargetsType], error) {
	var ok bool
	if trusted.snapshot == nil {
		return nil, fmt.Errorf("cannot load targets before snapshot")
	}
	// targets cannot be loaded if final snapshot is expired or its version
	// does not match meta version in timestamp
	err := trusted.checkFinalSnapshot()
	if err != nil {
		return nil, err
	}
	// check if delegator metadata is present
	if delegatorName == metadata.ROOT {
		if trusted.root != nil {
			ok = true
		} else {
			ok = false
		}
	} else {
		_, ok = trusted.targets[delegatorName]
	}
	if !ok {
		return nil, fmt.Errorf("cannot load targets before delegator")
	}
	fmt.Printf("updating %s delegated by %s\n", roleName, delegatorName)

	// Verify against the hashes in snapshot, if any
	meta, ok := trusted.snapshot.Signed.Meta[fmt.Sprintf("%s.json", roleName)]
	if !ok {
		return nil, fmt.Errorf("snapshot does not contain information for %s", roleName)
	}
	err = meta.VerifyLengthHashes(targetsData)
	if err != nil {
		return nil, err
	}
	newDelegate, err := metadata.Targets().FromBytes(targetsData)
	if err != nil {
		return nil, err
	}
	// check metadata type matches targets
	if newDelegate.Signed.Type != metadata.TARGETS {
		return nil, fmt.Errorf("expected %s, got %s", metadata.TARGETS, newDelegate.Signed.Type)
	}
	// get delegator metadata and verify the new delegatee
	if delegatorName == metadata.ROOT {
		err = trusted.root.VerifyDelegate(roleName, newDelegate)
		if err != nil {
			return nil, err
		}
	} else {
		err = trusted.targets[delegatorName].VerifyDelegate(roleName, newDelegate)
		if err != nil {
			return nil, err
		}
	}
	if newDelegate.Signed.Version != meta.Version {
		return nil, fmt.Errorf("expected %s version %d, got %d", roleName, meta.Version, newDelegate.Signed.Version)
	}
	if newDelegate.Signed.IsExpired(trusted.refTime) {
		return nil, fmt.Errorf("New %s is expired", roleName)
	}
	trusted.targets[roleName] = newDelegate
	fmt.Printf("Updated %s v%d\n", roleName, trusted.targets[roleName].Signed.Version)
	return trusted.targets[roleName], nil
}
