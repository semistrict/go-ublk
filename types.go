package ublk

// ctrlCmd mirrors `struct ublksrv_ctrl_cmd` from
// `include/uapi/linux/ublk_cmd.h`, shipped via sqe->cmd of io_uring SQE128
// commands to /dev/ublk-control.
type ctrlCmd struct {
	// DevID is the target device ID. Must be valid for all commands
	// except ADD_DEV (use ^uint32(0) to let the kernel assign one).
	DevID uint32
	// QueueID is the target queue. Must be ^uint16(0) (-1) if the
	// command is not queue-specific.
	QueueID uint16
	// Len is the byte length of the cmd-specific buffer at Addr.
	Len uint16
	// Addr points to a cmd-specific buffer (IN or OUT depending on cmd).
	Addr uint64
	// Data carries inline command data. For START_DEV, Data[0] is the
	// ublksrv daemon PID.
	Data [1]uint64
	// DevPathLen is the length of the char device path, including the
	// null terminator. Used for UBLK_F_UNPRIVILEGED_DEV and
	// UBLK_CMD_GET_DEV_INFO2 only.
	DevPathLen uint16
	Pad        uint16 // padding
	Reserved   uint32 // must be zero
}

// DevInfo mirrors `struct ublksrv_ctrl_dev_info` from
// `include/uapi/linux/ublk_cmd.h`. Exchanged between userspace and the kernel
// during ADD_DEV (written by userspace, updated by kernel) and GET_DEV_INFO
// (read from kernel).
type DevInfo struct {
	// NrHwQueues is the number of hardware IO queues.
	NrHwQueues uint16
	// QueueDepth is the maximum number of outstanding IOs per queue.
	QueueDepth uint16
	// State is the device state (StateDead, StateLive, StateQuiesced, StateFailIO).
	State uint16
	Pad0  uint16 // padding
	// MaxIOBufBytes is the maximum IO buffer size in bytes (max 32 MiB).
	MaxIOBufBytes uint32
	// DevID is the device ID. Set to ^uint32(0) for auto-assignment on ADD_DEV;
	// filled by kernel on return. Must match between ctrl_cmd.DevID and this field.
	DevID uint32
	// UblksrvPID is the PID of the ublk server process.
	UblksrvPID int32
	Pad1       uint32 // padding
	// Flags is a bitmask of UBLK_F_* feature flags (FlagUserCopy, FlagSupportZeroCopy, etc).
	Flags uint64
	// UblksrvFlags is for ublksrv internal use, invisible to the ublk driver.
	UblksrvFlags uint64
	// OwnerUID is the UID of the device creator, stored by the kernel.
	OwnerUID uint32
	// OwnerGID is the GID of the device creator, stored by the kernel.
	OwnerGID  uint32
	Reserved1 uint64 // must be zero
	Reserved2 uint64 // must be zero
}

// ioCmd mirrors `struct ublksrv_io_cmd` from
// `include/uapi/linux/ublk_cmd.h`, issued to the ublk driver via /dev/ublkcN
// as io_uring passthrough commands.
type ioCmd struct {
	// QID is the queue ID this IO command targets.
	QID uint16
	// Tag identifies which IO slot this command is for (used in
	// FETCH_REQ and COMMIT_AND_FETCH_REQ).
	Tag uint16
	// Result is the IO completion result in bytes. Valid for
	// COMMIT_AND_FETCH_REQ only; positive means bytes completed,
	// negative means -errno.
	Result int32
	// Addr is the userspace buffer address for FETCH commands.
	// Not used when UBLK_F_USER_COPY is enabled (userspace does
	// pread/pwrite on /dev/ublkcN instead). For UBLK_F_ZONED,
	// this union is reused to pass back the allocated LBA for
	// UBLK_IO_OP_ZONE_APPEND.
	Addr uint64
}

// ioDesc mirrors `struct ublksrv_io_desc` from
// `include/uapi/linux/ublk_cmd.h`, stored in the mmap'd shared command buffer
// and indexed by request tag. Written by the ublk driver, read by userspace
// after a FETCH command returns.
type ioDesc struct {
	// OpFlags packs the IO operation (bits 0-7) and IO flags (bits 8-31).
	// Use (OpFlags & 0xff) for the op and (OpFlags >> 8) for flags.
	OpFlags uint32
	// NrSectors is the number of 512-byte sectors for this IO.
	// For UBLK_IO_OP_REPORT_ZONES, this field is nr_zones instead.
	NrSectors uint32
	// StartSector is the starting sector (512-byte units) for this IO.
	StartSector uint64
	// Addr is the buffer address in the ublksrv daemon's VM space,
	// provided by the ublk driver.
	Addr uint64
}

const ioDescSize = 24 // sizeof(struct ublksrv_io_desc) from include/uapi/linux/ublk_cmd.h

// Params mirrors `struct ublk_params` from `include/uapi/linux/ublk_cmd.h`.
// Userspace must set Len for both SET_PARAMS and GET_PARAMS. The driver may
// update Len and Types if the two sides use different versions.
type Params struct {
	// Len is the total byte length of this struct. Must be set by userspace.
	Len uint32
	// Types is a bitmask of UBLK_PARAM_TYPE_* indicating which parameter
	// sub-structs are included (ParamTypeBasic, ParamTypeDiscard, etc).
	Types uint32
	// Basic contains fundamental block device parameters. Included when
	// Types has ParamTypeBasic set.
	Basic ParamBasic
	// Discard contains discard/trim and write-zeroes parameters. Included
	// when Types has ParamTypeDiscard set.
	Discard ParamDiscard
	// Devt contains the device major/minor numbers. Read-only; included
	// when Types has ParamTypeDevt set. Available after START_DEV.
	Devt ParamDevt
	// Zoned contains zoned storage parameters. Included when Types has
	// ParamTypeZoned set.
	Zoned ParamZoned
	// DMA contains DMA alignment requirements. Included when Types has
	// ParamTypeDMAAlign set.
	DMA ParamDMAAlign
	// Seg contains segment size constraints. Included when Types has
	// ParamTypeSegment set.
	Seg ParamSegment
	// Integrity contains integrity/metadata parameters. Included when
	// Types has ParamTypeIntegrity set. Requires UBLK_F_INTEGRITY.
	Integrity ParamIntegrity
}

// ParamBasic mirrors `struct ublk_param_basic` from
// `include/uapi/linux/ublk_cmd.h`.
type ParamBasic struct {
	// Attrs is a bitmask of device attributes (AttrReadOnly, AttrRotational,
	// AttrVolatileCache, AttrFUA).
	Attrs uint32
	// LogicalBSShift is log2 of the logical block size (e.g. 9 for 512 bytes).
	LogicalBSShift uint8
	// PhysicalBSShift is log2 of the physical block size (e.g. 12 for 4096 bytes).
	PhysicalBSShift uint8
	// IOOptShift is log2 of the optimal IO size.
	IOOptShift uint8
	// IOMinShift is log2 of the minimum IO size.
	IOMinShift uint8
	// MaxSectors is the maximum number of 512-byte sectors per IO request.
	MaxSectors uint32
	// ChunkSectors is the chunk size in sectors for striped devices (0 = no chunking).
	ChunkSectors uint32
	// DevSectors is the device size in 512-byte sectors.
	DevSectors uint64
	// VirtBoundaryMask is the virtual boundary mask for segment alignment.
	VirtBoundaryMask uint64
}

// ParamDiscard mirrors `struct ublk_param_discard` from
// `include/uapi/linux/ublk_cmd.h`.
type ParamDiscard struct {
	// DiscardAlignment is the byte offset of the first discardable sector.
	DiscardAlignment uint32
	// DiscardGranularity is the minimum discard size in bytes.
	DiscardGranularity uint32
	// MaxDiscardSectors is the maximum discard size in 512-byte sectors.
	MaxDiscardSectors uint32
	// MaxWriteZeroesSectors is the maximum write-zeroes size in 512-byte sectors.
	MaxWriteZeroesSectors uint32
	// MaxDiscardSegments is the maximum number of discard segments per request.
	MaxDiscardSegments uint16
	Reserved0          uint16 // padding
}

// ParamDevt mirrors `struct ublk_param_devt` from
// `include/uapi/linux/ublk_cmd.h`. Read-only; cannot be set via SET_PARAMS.
// The disk_major/disk_minor are available after the device is started.
type ParamDevt struct {
	// CharMajor is the major number of the /dev/ublkcN char device.
	CharMajor uint32
	// CharMinor is the minor number of the /dev/ublkcN char device.
	CharMinor uint32
	// DiskMajor is the major number of the /dev/ublkbN block device.
	DiskMajor uint32
	// DiskMinor is the minor number of the /dev/ublkbN block device.
	DiskMinor uint32
}

// ParamZoned mirrors `struct ublk_param_zoned` from
// `include/uapi/linux/ublk_cmd.h`.
type ParamZoned struct {
	// MaxOpenZones is the maximum number of simultaneously open zones.
	MaxOpenZones uint32
	// MaxActiveZones is the maximum number of simultaneously active zones.
	MaxActiveZones uint32
	// MaxZoneAppendSectors is the maximum zone-append size in 512-byte sectors.
	MaxZoneAppendSectors uint32
	Reserved             [20]byte // must be zero
}

// ParamDMAAlign mirrors `struct ublk_param_dma_align` from
// `include/uapi/linux/ublk_cmd.h`.
type ParamDMAAlign struct {
	// Alignment is the required DMA alignment in bytes.
	Alignment uint32
	Pad       [4]byte // padding
}

// ParamSegment mirrors `struct ublk_param_segment` from
// `include/uapi/linux/ublk_cmd.h`. If any of the three parameters is zero,
// behavior is undefined.
type ParamSegment struct {
	// SegBoundaryMask defines segment boundaries. (SegBoundaryMask + 1)
	// must be a power of 2 and >= 4096 (UBLK_MIN_SEGMENT_SIZE).
	SegBoundaryMask uint64
	// MaxSegmentSize is the maximum segment size in bytes. May be
	// overridden by VirtBoundaryMask. Must be >= 4096.
	MaxSegmentSize uint32
	// MaxSegments is the maximum number of segments per IO request.
	MaxSegments uint16
	Pad         [2]byte // padding
}

// ParamIntegrity mirrors `struct ublk_param_integrity` from
// `include/uapi/linux/ublk_cmd.h`. Requires UBLK_F_INTEGRITY and
// UBLK_F_USER_COPY.
type ParamIntegrity struct {
	// Flags contains LBMD_PI_CAP_* flags from linux/fs.h.
	Flags uint32
	// MaxIntegritySegments is the max number of integrity segments (0 = no limit).
	MaxIntegritySegments uint16
	// IntervalExp is log2 of the integrity interval size.
	IntervalExp uint8
	// MetadataSize is the metadata size per interval. Must be nonzero
	// when UBLK_PARAM_TYPE_INTEGRITY is set.
	MetadataSize uint8
	// PIOffset is the byte offset of protection information within metadata.
	PIOffset uint8
	// CsumType is the checksum type (LBMD_PI_CSUM_* from linux/fs.h).
	CsumType uint8
	// TagSize is the size of the reference tag in bytes.
	TagSize uint8
	Pad     [5]byte // padding
}
