package ublk

import "unsafe"

// Control commands — legacy numeric opcodes from UBLK_CMD_*.
// These are the _IOC_NR values. On kernels compiled without
// CONFIG_BLKDEV_UBLK_LEGACY_OPCODES, only the ioctl-encoded
// variants (UBLK_U_CMD_*) are accepted.
const (
	CmdGetQueueAffinity = 0x01 // retrieve per-queue CPU affinity
	CmdGetDevInfo       = 0x02 // retrieve device info
	CmdAddDev           = 0x04 // allocate a new ublk device (creates /dev/ublkcN)
	CmdDelDev           = 0x05 // remove a ublk device (synchronous)
	CmdStartDev         = 0x06 // expose the block device (/dev/ublkbN)
	CmdStopDev          = 0x07 // halt and remove the block device
	CmdSetParams        = 0x08 // set device parameters (before START_DEV)
	CmdGetParams        = 0x09 // retrieve device parameters
	CmdStartUserRecov   = 0x10 // begin user-initiated recovery after server exit
	CmdEndUserRecov     = 0x11 // complete user-initiated recovery
	CmdGetDevInfo2      = 0x12 // retrieve device info with unprivileged audit path
	CmdGetFeatures      = 0x13 // query supported kernel features
	CmdDelDevAsync      = 0x14 // remove a ublk device (asynchronous)
	CmdUpdateSize       = 0x15 // resize a live device (requires UBLK_F_UPDATE_SIZE)
	CmdQuiesceDev       = 0x16 // quiesce device for live server upgrade
	CmdTryStopDev       = 0x17 // stop only if no openers (requires UBLK_F_SAFE_STOP_DEV)
)

// IO commands — legacy numeric opcodes from UBLK_IO_*.
// Issued via io_uring passthrough (IORING_OP_URING_CMD) on /dev/ublkcN.
const (
	IoFetchReq          = 0x20 // fetch next IO request from driver
	IoCommitAndFetchReq = 0x21 // commit result of current IO and fetch next
	IoNeedGetData       = 0x22 // request data copy for write (UBLK_F_NEED_GET_DATA only)
	IoRegisterIOBuf     = 0x23 // register request buffer for zero-copy
	IoUnregisterIOBuf   = 0x24 // unregister request buffer
)

// Linux ioctl encoding: dir(2) | size(14) | type(8) | nr(8).
// See include/uapi/asm-generic/ioctl.h.
const (
	iocNone  = 0 // no data transfer
	iocWrite = 1 // userspace writes, kernel reads
	iocRead  = 2 // kernel writes, userspace reads

	iocNrBits   = 8  // bits for command number
	iocTypeBits = 8  // bits for device type character
	iocSizeBits = 14 // bits for argument struct size
	iocDirBits  = 2  // bits for data direction

	iocNrShift   = 0
	iocTypeShift = iocNrShift + iocNrBits     // 8
	iocSizeShift = iocTypeShift + iocTypeBits // 16
	iocDirShift  = iocSizeShift + iocSizeBits // 30
)

// ioc builds a Linux ioctl number from direction, type char, command number, and arg size.
func ioc(dir, typ, nr, size uint32) uint32 {
	return (dir << iocDirShift) | (typ << iocTypeShift) | (nr << iocNrShift) | (size << iocSizeShift)
}

func ior(typ, nr, size uint32) uint32  { return ioc(iocRead, typ, nr, size) }          // _IOR
func iowr(typ, nr, size uint32) uint32 { return ioc(iocRead|iocWrite, typ, nr, size) } // _IOWR

// ublkType is the ioctl type character for ublk commands ('u' = 0x75).
const ublkType = 'u'

var (
	ctrlCmdSize = uint32(unsafe.Sizeof(ctrlCmd{})) // sizeof(ublksrv_ctrl_cmd) = 32
	ioCmdSize   = uint32(unsafe.Sizeof(ioCmd{}))   // sizeof(ublksrv_io_cmd) = 16
)

// Ioctl-encoded control commands (UBLK_U_CMD_*). These encode the command
// number, data direction, and payload size into a single uint32 using the
// Linux _IOR/_IOWR macros with type 'u'.
var (
	ublkUCmdGetQueueAffinity = ior(ublkType, CmdGetQueueAffinity, ctrlCmdSize)
	ublkUCmdGetDevInfo       = ior(ublkType, CmdGetDevInfo, ctrlCmdSize)
	ublkUCmdAddDev           = iowr(ublkType, CmdAddDev, ctrlCmdSize)
	ublkUCmdDelDev           = iowr(ublkType, CmdDelDev, ctrlCmdSize)
	ublkUCmdStartDev         = iowr(ublkType, CmdStartDev, ctrlCmdSize)
	ublkUCmdStopDev          = iowr(ublkType, CmdStopDev, ctrlCmdSize)
	ublkUCmdSetParams        = iowr(ublkType, CmdSetParams, ctrlCmdSize)
	ublkUCmdGetParams        = ior(ublkType, CmdGetParams, ctrlCmdSize)
	ublkUCmdStartUserRecov   = iowr(ublkType, CmdStartUserRecov, ctrlCmdSize)
	ublkUCmdEndUserRecov     = iowr(ublkType, CmdEndUserRecov, ctrlCmdSize)
	ublkUCmdGetDevInfo2      = ior(ublkType, CmdGetDevInfo2, ctrlCmdSize)
	ublkUCmdGetFeatures      = ior(ublkType, CmdGetFeatures, ctrlCmdSize)
	ublkUCmdDelDevAsync      = ior(ublkType, CmdDelDevAsync, ctrlCmdSize)
	ublkUCmdUpdateSize       = iowr(ublkType, CmdUpdateSize, ctrlCmdSize)
	ublkUCmdQuiesceDev       = iowr(ublkType, CmdQuiesceDev, ctrlCmdSize)
	ublkUCmdTryStopDev       = iowr(ublkType, CmdTryStopDev, ctrlCmdSize)
)

// Ioctl-encoded IO commands (UBLK_U_IO_*). Same encoding scheme as control
// commands but used on /dev/ublkcN for IO operations.
var (
	ublkUIoFetchReq          = iowr(ublkType, IoFetchReq, ioCmdSize)
	ublkUIoCommitAndFetchReq = iowr(ublkType, IoCommitAndFetchReq, ioCmdSize)
	ublkUIoNeedGetData       = iowr(ublkType, IoNeedGetData, ioCmdSize)
	ublkUIoRegisterIOBuf     = iowr(ublkType, IoRegisterIOBuf, ioCmdSize)
	ublkUIoUnregisterIOBuf   = iowr(ublkType, IoUnregisterIOBuf, ioCmdSize)
)

// Keep the full mirrored ioctl surface available even before every command is
// wired up so the numeric definitions stay aligned with the kernel headers.
var (
	_ = ublkUCmdGetDevInfo
	_ = ublkUCmdStartUserRecov
	_ = ublkUCmdEndUserRecov
	_ = ublkUCmdGetDevInfo2
	_ = ublkUCmdGetFeatures
	_ = ublkUCmdDelDevAsync
	_ = ublkUCmdUpdateSize
	_ = ublkUCmdQuiesceDev
	_ = ublkUCmdTryStopDev
	_ = ublkUIoNeedGetData
	_ = ublkUIoRegisterIOBuf
	_ = ublkUIoUnregisterIOBuf
)

// IO result codes returned in CQE.Res for IO commands.
const (
	IOResOK          = 0          // IO completed successfully
	IOResNeedGetData = 1          // server must issue NEED_GET_DATA to get write data
	IOResAbort       = -int32(19) // -ENODEV: device is being torn down, no re-fetch
)

// Buffer layout constants. The ublk driver maps a shared memory region for
// each queue with the following layout:
//
//	[0, UBLKSRV_IO_BUF_OFFSET)              — command buffer (ublksrv_io_desc array)
//	[UBLKSRV_IO_BUF_OFFSET, ...)            — IO data buffers (only for non-USER_COPY mode)
//
// IO buffer addresses are computed as:
//
//	UBLKSRV_IO_BUF_OFFSET + (tag << IO_BUF_BITS) + (qid << (IO_BUF_BITS + TAG_BITS))
const (
	ublkSrvCmdBufOffset = 0          // start of per-queue command buffer (mmap offset)
	ublkSrvIOBufOffset  = 0x80000000 // start of IO data buffers (2 GiB)

	ublkMaxQueueDepth = 4096 // maximum queue depth (tag is 16-bit but limited to 4096)

	ublkIOBufBits = 25 // bits for IO buffer offset (max 32 MiB per buffer)
	ublkTagBits   = 16 // bits for request tag (max 64K IOs per queue)
	ublkQIDBits   = 12 // bits for queue ID (max 4096 queues)
)

// Feature flags (UBLK_F_*) set in DevInfo.Flags to request kernel features.
// The kernel rejects ADD_DEV if unknown flags are set.
const (
	FlagSupportZeroCopy     = 1 << 0  // enable zero-copy via io_uring fixed buffers (requires CAP_SYS_ADMIN)
	FlagUringCmdCompInTask  = 1 << 1  // force io command completion via task_work (for benchmarking)
	FlagNeedGetData         = 1 << 2  // server must issue NEED_GET_DATA for write requests
	FlagUserRecovery        = 1 << 3  // device survives server exit; outstanding IO gets errors
	FlagUserRecoveryReissue = 1 << 4  // device survives server exit; outstanding IO is reissued
	FlagUnprivilegedDev     = 1 << 5  // allow unprivileged users to create/manage the device
	FlagCmdIoctlEncode      = 1 << 6  // IO commands use ioctl-style encoding (not legacy opcodes)
	FlagUserCopy            = 1 << 7  // data transfer via pread/pwrite on /dev/ublkcN (no shared buffer)
	FlagZoned               = 1 << 8  // device supports zoned storage operations
	FlagUserRecoveryFailIO  = 1 << 9  // device survives server exit; all IO fails immediately
	FlagUpdateSize          = 1 << 10 // allow live resize via UBLK_U_CMD_UPDATE_SIZE
	FlagAutoBufReg          = 1 << 11 // auto-register request buffers in io_uring fixed buffer table
	FlagQuiesce             = 1 << 12 // support UBLK_U_CMD_QUIESCE_DEV for live server upgrades
	FlagPerIODaemon         = 1 << 13 // each (qid,tag) pair can have its own daemon task
	FlagBufRegOffDaemon     = 1 << 14 // REGISTER/UNREGISTER_IO_BUF can be issued from any task
	FlagBatchIO             = 1 << 15 // support batch IO commands (PREP/COMMIT/FETCH_IO_CMDS)
	FlagIntegrity           = 1 << 16 // device supports integrity/metadata buffers (requires USER_COPY)
	FlagSafeStopDev         = 1 << 17 // support TRY_STOP_DEV (stop only if no openers)
	FlagNoAutoPartScan      = 1 << 18 // disable automatic partition scanning on START_DEV
)

// Device states (UBLK_S_DEV_*) reported in DevInfo.State.
const (
	StateDead     = 0 // device is not active
	StateLive     = 1 // device is serving IO
	StateQuiesced = 2 // device is quiesced (IO paused for server upgrade)
	StateFailIO   = 3 // device is failing all IO (recovery in progress)
)

// IOOp identifies the block IO operation. Stored in bits 0-7 of ioDesc.OpFlags.
type IOOp uint8

const (
	OpRead         IOOp = 0  // read sectors
	OpWrite        IOOp = 1  // write sectors
	OpFlush        IOOp = 2  // flush volatile cache
	OpDiscard      IOOp = 3  // discard/trim sectors (deallocate)
	OpWriteSame    IOOp = 4  // write same data to multiple sectors
	OpWriteZeroes  IOOp = 5  // write zeroes (may deallocate)
	OpZoneOpen     IOOp = 10 // open a zone (zoned storage)
	OpZoneClose    IOOp = 11 // close a zone
	OpZoneFinish   IOOp = 12 // finish a zone (transition to full)
	OpZoneAppend   IOOp = 13 // append write to a zone
	OpZoneResetAll IOOp = 14 // reset all zones to empty
	OpZoneReset    IOOp = 15 // reset a single zone to empty
	OpReportZones  IOOp = 18 // report zone descriptors
)

func (op IOOp) String() string {
	switch op {
	case OpRead:
		return "READ"
	case OpWrite:
		return "WRITE"
	case OpFlush:
		return "FLUSH"
	case OpDiscard:
		return "DISCARD"
	case OpWriteSame:
		return "WRITE_SAME"
	case OpWriteZeroes:
		return "WRITE_ZEROES"
	case OpZoneOpen:
		return "ZONE_OPEN"
	case OpZoneClose:
		return "ZONE_CLOSE"
	case OpZoneFinish:
		return "ZONE_FINISH"
	case OpZoneAppend:
		return "ZONE_APPEND"
	case OpZoneResetAll:
		return "ZONE_RESET_ALL"
	case OpZoneReset:
		return "ZONE_RESET"
	case OpReportZones:
		return "REPORT_ZONES"
	default:
		return "UNKNOWN"
	}
}

// IO flags (UBLK_IO_F_*) stored in bits 8-31 of ioDesc.OpFlags.
const (
	IOFlagFailfastDev       = 1 << 8  // fail fast on device error
	IOFlagFailfastTransport = 1 << 9  // fail fast on transport error
	IOFlagFailfastDriver    = 1 << 10 // fail fast on driver error
	IOFlagMeta              = 1 << 11 // request carries metadata
	IOFlagFUA               = 1 << 13 // force unit access (bypass volatile cache)
	IOFlagNoUnmap           = 1 << 15 // discard should not unmap (write zeroes instead)
	IOFlagSwap              = 1 << 16 // IO is from the swap subsystem
	IOFlagNeedRegBuf        = 1 << 17 // auto-buf-reg failed; server must register manually
	IOFlagIntegrity         = 1 << 18 // request has an integrity/metadata buffer
)

// Parameter type flags for Params.Types, selecting which sub-structs are active.
const (
	ParamTypeBasic     = 1 << 0 // ParamBasic: block size, device size, max sectors
	ParamTypeDiscard   = 1 << 1 // ParamDiscard: discard/trim and write-zeroes limits
	ParamTypeDevt      = 1 << 2 // ParamDevt: char/block device major/minor (read-only)
	ParamTypeZoned     = 1 << 3 // ParamZoned: zone limits for zoned storage
	ParamTypeDMAAlign  = 1 << 4 // ParamDMAAlign: DMA alignment requirement
	ParamTypeSegment   = 1 << 5 // ParamSegment: segment size/count constraints
	ParamTypeIntegrity = 1 << 6 // ParamIntegrity: integrity metadata (requires UBLK_F_INTEGRITY)

	ParamTypeAll = ParamTypeBasic | ParamTypeDiscard | ParamTypeDevt |
		ParamTypeZoned | ParamTypeDMAAlign | ParamTypeSegment | ParamTypeIntegrity
)

// Basic parameter attributes for ParamBasic.Attrs (UBLK_ATTR_*).
const (
	AttrReadOnly      = 1 << 0 // device is read-only
	AttrRotational    = 1 << 1 // device is rotational (affects IO scheduling)
	AttrVolatileCache = 1 << 2 // device has a volatile write cache (flush matters)
	AttrFUA           = 1 << 3 // device supports Force Unit Access
)
