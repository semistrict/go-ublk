package ublk

import (
	"fmt"
	"os"
	"time"
)

const (
	// queueShutdownGracePeriod gives STOP_DEV time to abort pending queue work
	// before Delete falls back to interrupting the queue rings.
	queueShutdownGracePeriod = 2 * time.Second

	// defaultHardwareQueues matches the common single-queue default for simple devices.
	defaultHardwareQueues = 1
	// defaultQueueDepth is the standard queue depth used when the caller leaves it unset.
	defaultQueueDepth = 128
	// defaultMaxIOBufBytes matches the kernel-friendly 512 KiB user-copy buffer size.
	defaultMaxIOBufBytes = 512 * 1024
	// controlRingEntries is the small SQE128 ring size needed for control commands.
	controlRingEntries = 4
	// charDevicePollAttempts bounds the udev/device-node retry loop during device creation.
	charDevicePollAttempts = 50
	// zeroCopyRingDepthMultiplier leaves room for target IO plus commit/fetch commands per queue depth slot.
	zeroCopyRingDepthMultiplier = 4
)

type deviceHooks struct {
	createControlResources func() (*os.File, *ioURing, error)
	addControlDevice       func(*Device, *DevInfo) error
	openCharDevice         func(*Device, string) (*os.File, error)
	sleep                  func(time.Duration)
	setParams              func(*Device, *Params) error
	getParams              func(*Device, *Params) error
	prepareUserQueue       func(*Device, uint16) (*preparedUserQueue, error)
	prepareZeroCopyQueue   func(*Device, uint16) (*preparedZeroCopyQueue, error)
	startControl           func(*Device) error
	stopControl            func(*Device) error
	stopControlWait        func(*Device) error
	deleteControl          func(*Device) error
	closeChar              func(*Device) error
	closeCtrlRing          func(*Device) error
	closeCtrlFile          func(*Device) error
	waitServe              func(*Device, time.Duration) bool
	interruptIORings       func(*Device)
	applyQueueAffinity     func(*Device, uint16)
}

type preparedUserQueue struct {
	ring    queueRing
	cmdBuf  []byte
	release func()
}

type preparedZeroCopyQueue struct {
	ring    queueRing
	cmdBuf  []byte
	charFD  int32
	release func()
}

func normalizeDeviceOptions(opts DeviceOptions) DeviceOptions {
	if opts.Queues == 0 {
		opts.Queues = defaultHardwareQueues
	}
	if opts.QueueDepth == 0 {
		opts.QueueDepth = defaultQueueDepth
	}
	if opts.MaxIOBufBytes == 0 {
		opts.MaxIOBufBytes = defaultMaxIOBufBytes
	}
	if opts.Flags&FlagSupportZeroCopy != 0 {
		// Zero-copy mode uses AUTO_BUF_REG and does not use user-copy buffers.
		opts.Flags |= FlagAutoBufReg | FlagCmdIoctlEncode
		opts.Flags &^= FlagUserCopy
		return opts
	}
	opts.Flags |= FlagUserCopy | FlagCmdIoctlEncode
	return opts
}

func newDeviceInfo(opts DeviceOptions) DevInfo {
	return DevInfo{
		NrHwQueues:    opts.Queues,
		QueueDepth:    opts.QueueDepth,
		MaxIOBufBytes: opts.MaxIOBufBytes,
		DevID:         ^uint32(0), // -1: let kernel assign
		UblksrvPID:    int32(os.Getpid()),
		Flags:         opts.Flags,
	}
}

func blockDevicePathForID(id int32) string {
	return fmt.Sprintf("/dev/ublkb%d", id)
}

func charDevicePathForID(id int32) string {
	return fmt.Sprintf("/dev/ublkc%d", id)
}

func (d *Device) createControlResources() (*os.File, *ioURing, error) {
	if d.hooks != nil && d.hooks.createControlResources != nil {
		return d.hooks.createControlResources()
	}

	ctrlFile, err := os.OpenFile(controlDevPath, os.O_RDWR, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open %s: %w", controlDevPath, err)
	}

	ring, err := newIOURing(controlRingEntries, true)
	if err != nil {
		_ = ctrlFile.Close()
		return nil, nil, fmt.Errorf("create control io_uring: %w", err)
	}
	return ctrlFile, ring, nil
}

func (d *Device) addControlDevice(info *DevInfo) error {
	if d.hooks != nil && d.hooks.addControlDevice != nil {
		return d.hooks.addControlDevice(d, info)
	}
	return d.ctrlAddDev(info)
}

func (d *Device) openCharDevice(path string) (*os.File, error) {
	if d.hooks != nil && d.hooks.openCharDevice != nil {
		return d.hooks.openCharDevice(d, path)
	}
	return os.OpenFile(path, os.O_RDWR, 0)
}

func (d *Device) sleep(delay time.Duration) {
	if d.hooks != nil && d.hooks.sleep != nil {
		d.hooks.sleep(delay)
		return
	}
	time.Sleep(delay)
}

func (d *Device) setParamsControl(params *Params) error {
	if d.hooks != nil && d.hooks.setParams != nil {
		return d.hooks.setParams(d, params)
	}
	return d.ctrlSetParams(params)
}

func (d *Device) getParamsControl(params *Params) error {
	if d.hooks != nil && d.hooks.getParams != nil {
		return d.hooks.getParams(d, params)
	}
	return d.ctrlGetParams(params)
}

func (d *Device) startControl() error {
	if d.hooks != nil && d.hooks.startControl != nil {
		return d.hooks.startControl(d)
	}
	return d.ctrlStartDev()
}

func (d *Device) stopControl() error {
	if d.hooks != nil && d.hooks.stopControl != nil {
		return d.hooks.stopControl(d)
	}
	return d.ctrlStopDev()
}

func (d *Device) stopControlWait() error {
	if d.hooks != nil && d.hooks.stopControlWait != nil {
		return d.hooks.stopControlWait(d)
	}
	return d.ctrlStopDevWait()
}

func (d *Device) deleteControl() error {
	if d.hooks != nil && d.hooks.deleteControl != nil {
		return d.hooks.deleteControl(d)
	}
	return d.ctrlDelDev()
}

func (d *Device) closeCharDevice() error {
	if d.hooks != nil && d.hooks.closeChar != nil {
		return d.hooks.closeChar(d)
	}
	if d.charFile == nil {
		return nil
	}
	err := d.charFile.Close()
	d.charFile = nil
	return err
}

func (d *Device) closeControlRing() error {
	if d.hooks != nil && d.hooks.closeCtrlRing != nil {
		return d.hooks.closeCtrlRing(d)
	}
	if d.ctrlRing == nil {
		return nil
	}
	return d.ctrlRing.Close()
}

func (d *Device) closeControlFile() error {
	if d.hooks != nil && d.hooks.closeCtrlFile != nil {
		return d.hooks.closeCtrlFile(d)
	}
	if d.ctrlFile == nil {
		return nil
	}
	return d.ctrlFile.Close()
}

func (d *Device) waitServeWithTimeout(timeout time.Duration) bool {
	if d.hooks != nil && d.hooks.waitServe != nil {
		return d.hooks.waitServe(d, timeout)
	}
	return d.waitServe(timeout)
}

func (d *Device) interruptQueueRings() {
	if d.hooks != nil && d.hooks.interruptIORings != nil {
		d.hooks.interruptIORings(d)
		return
	}
	d.ioRingsMu.Lock()
	for _, ring := range d.ioRings {
		if ring != nil {
			ring.Interrupt()
		}
	}
	d.ioRingsMu.Unlock()
}

func (d *Device) applyQueueAffinity(qid uint16) {
	if d.hooks != nil && d.hooks.applyQueueAffinity != nil {
		d.hooks.applyQueueAffinity(d, qid)
		return
	}
	d.setQueueAffinity(qid)
}
