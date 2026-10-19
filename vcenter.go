package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/view"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

// logAction пишет в stdout запись об изменяющем действии: кто (логин
// подключения), где (адрес vCenter) и что сделал. Это единственный журнал
// действий инструмента — он не хранит собственной истории.
//
// logAction writes a line to stdout for a state-changing action: who (the
// connection's login), where (the vCenter address) and what was done. This
// is the tool's only action log — it keeps no history of its own.
func logAction(conn *Connection, format string, args ...interface{}) {
	prefix := fmt.Sprintf("[%s @ %s] ", conn.Username, conn.Address)
	log.Printf(prefix+format, args...)
}

// Connection представляет одно активное подключение к vCenter.
//
// Connection represents one active vCenter connection.
type Connection struct {
	ID       string
	Address  string
	Username string
	Insecure bool
	Client   *govmomi.Client
}

// VCenterInfo — публичное представление подключения для фронтенда.
//
// VCenterInfo is the connection's public representation sent to the frontend.
type VCenterInfo struct {
	ID       string `json:"id"`
	Address  string `json:"address"`
	Username string `json:"username"`
}

// VMSummary — краткая информация о ВМ в результатах поиска.
//
// VMSummary is a short VM description used in search results.
type VMSummary struct {
	Ref        string `json:"ref"` // moref, например vm-1234 / moref, e.g. vm-1234
	Name       string `json:"name"`
	PowerState string `json:"powerState"`
	Folder     string `json:"folder"`
}

// DiskInfo — информация о диске ВМ и его текущем лимите IOPS.
//
// DiskInfo describes one VM disk and its current IOPS limit.
type DiskInfo struct {
	Key         int32   `json:"key"`
	Label       string  `json:"label"`
	SizeGB      float64 `json:"sizeGB"`
	Limit       int64   `json:"limit"`       // -1 = unlimited
	SCSIAddress string  `json:"scsiAddress"` // "bus:unit", например "0:0"; пусто, если контроллер не определён / "bus:unit", e.g. "0:0"; empty if the controller is unknown
	Controller  string  `json:"controller"`  // например "LSI Logic SAS", "VMware Paravirtual", "NVMe" / e.g. "LSI Logic SAS", "VMware Paravirtual", "NVMe"
}

// Manager хранит все активные подключения к vCenter.
//
// Manager holds all active vCenter connections.
type Manager struct {
	mu    sync.Mutex
	conns map[string]*Connection
}

// NewManager создаёт пустой Manager.
//
// NewManager creates an empty Manager.
func NewManager() *Manager {
	return &Manager{conns: make(map[string]*Connection)}
}

// newID генерирует случайный идентификатор подключения.
//
// newID generates a random connection identifier.
func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Connect устанавливает новое подключение к vCenter по адресу/логину/паролю
// и сохраняет его в Manager под новым идентификатором.
//
// Connect establishes a new vCenter connection from address/username/password
// and stores it in the Manager under a fresh identifier.
func (m *Manager) Connect(ctx context.Context, address, username, password string, insecure bool) (*VCenterInfo, error) {
	address = strings.TrimSpace(address)
	if !strings.Contains(address, "://") {
		address = "https://" + address
	}
	if !strings.HasSuffix(address, "/sdk") {
		address = strings.TrimRight(address, "/") + "/sdk"
	}

	u, err := url.Parse(address)
	if err != nil {
		return nil, fmt.Errorf("некорректный адрес vCenter: %w", err)
	}
	u.User = url.UserPassword(username, password)

	client, err := govmomi.NewClient(ctx, u, insecure)
	if err != nil {
		return nil, fmt.Errorf("ошибка подключения: %w", err)
	}

	id := newID()
	conn := &Connection{
		ID:       id,
		Address:  u.Hostname(),
		Username: username,
		Insecure: insecure,
		Client:   client,
	}

	m.mu.Lock()
	m.conns[id] = conn
	m.mu.Unlock()

	logAction(conn, "подключение установлено (SSL-проверка %s)", map[bool]string{true: "отключена", false: "включена"}[insecure])

	return &VCenterInfo{ID: id, Address: conn.Address, Username: username}, nil
}

// Disconnect удаляет подключение из Manager и завершает сессию на стороне
// vCenter. Запись удаляется независимо от результата Logout, поэтому
// подключение с уже недействительной сессией всё равно считается закрытым.
//
// Disconnect removes the connection from the Manager and logs out of
// vCenter. The entry is removed regardless of the Logout outcome, so a
// connection whose session is already invalid is still treated as closed.
func (m *Manager) Disconnect(ctx context.Context, id string) error {
	m.mu.Lock()
	conn, ok := m.conns[id]
	if ok {
		delete(m.conns, id)
	}
	m.mu.Unlock()

	if !ok {
		return fmt.Errorf("подключение не найдено")
	}
	logAction(conn, "отключение")

	if err := conn.Client.Logout(ctx); err != nil {
		log.Printf("[%s @ %s] logout завершился с ошибкой (сессия могла уже истечь), соединение всё равно удалено: %v",
			conn.Username, conn.Address, err)
	}
	return nil
}

// List возвращает все активные подключения, отсортированные по адресу.
//
// List returns all active connections, sorted by address.
func (m *Manager) List() []VCenterInfo {
	m.mu.Lock()
	defer m.mu.Unlock()

	list := make([]VCenterInfo, 0, len(m.conns))
	for _, c := range m.conns {
		list = append(list, VCenterInfo{ID: c.ID, Address: c.Address, Username: c.Username})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Address < list[j].Address })
	return list
}

// Get возвращает подключение по идентификатору.
//
// Get looks up a connection by its identifier.
func (m *Manager) Get(id string) (*Connection, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.conns[id]
	return c, ok
}

// SearchVMs ищет ВМ по подстроке имени либо по точному moref/vm-id.
//
// SearchVMs finds VMs by a name substring, or by an exact moref/vm-id.
func (m *Manager) SearchVMs(ctx context.Context, conn *Connection, query string, byID bool) ([]VMSummary, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("пустой поисковый запрос")
	}

	client := conn.Client.Client

	if byID {
		ref := normalizeVMRef(query)
		var vmMo mo.VirtualMachine
		err := mo.RetrieveProperties(ctx, client, client.ServiceContent.PropertyCollector, ref, &vmMo)
		if err != nil {
			return nil, fmt.Errorf("ВМ с ID %q не найдена: %w", query, err)
		}
		return []VMSummary{{
			Ref:        ref.Value,
			Name:       vmMo.Name,
			PowerState: string(vmMo.Runtime.PowerState),
		}}, nil
	}

	m2 := view.NewManager(client)
	cv, err := m2.CreateContainerView(ctx, client.ServiceContent.RootFolder, []string{"VirtualMachine"}, true)
	if err != nil {
		return nil, err
	}
	defer cv.Destroy(ctx)

	var vms []mo.VirtualMachine
	err = cv.Retrieve(ctx, []string{"VirtualMachine"}, []string{"name", "runtime.powerState"}, &vms)
	if err != nil {
		return nil, err
	}

	lowerQuery := strings.ToLower(query)
	var results []VMSummary
	for _, v := range vms {
		if strings.Contains(strings.ToLower(v.Name), lowerQuery) {
			results = append(results, VMSummary{
				Ref:        v.Self.Value,
				Name:       v.Name,
				PowerState: string(v.Runtime.PowerState),
			})
		}
	}
	sort.Slice(results, func(i, j int) bool { return results[i].Name < results[j].Name })

	if len(results) > 200 {
		results = results[:200]
	}
	return results, nil
}

// normalizeVMRef приводит пользовательский ввод VM ID к types.ManagedObjectReference.
//
// normalizeVMRef converts a user-supplied VM ID into a types.ManagedObjectReference.
func normalizeVMRef(id string) types.ManagedObjectReference {
	id = strings.TrimSpace(id)
	if strings.HasPrefix(id, "VirtualMachine:") {
		id = strings.TrimPrefix(id, "VirtualMachine:")
	}
	if strings.HasPrefix(id, "VirtualMachine-") {
		id = strings.TrimPrefix(id, "VirtualMachine-")
	}
	return types.ManagedObjectReference{Type: "VirtualMachine", Value: id}
}

// vmObject строит объект govmomi для ВМ по её moref-значению.
//
// vmObject builds a govmomi object for a VM from its moref value.
func vmObject(conn *Connection, ref string) *object.VirtualMachine {
	moRef := types.ManagedObjectReference{Type: "VirtualMachine", Value: ref}
	return object.NewVirtualMachine(conn.Client.Client, moRef)
}

// GetDisks возвращает список дисков ВМ с текущими лимитами IOPS, SCSI-адресом
// и типом контроллера.
//
// GetDisks lists a VM's disks with their current IOPS limits, SCSI address
// and controller type.
func (m *Manager) GetDisks(ctx context.Context, conn *Connection, vmRef string) ([]DiskInfo, error) {
	vm := vmObject(conn, vmRef)

	var vmMo mo.VirtualMachine
	err := vm.Properties(ctx, vm.Reference(), []string{"config.hardware.device"}, &vmMo)
	if err != nil {
		return nil, fmt.Errorf("не удалось получить конфигурацию ВМ: %w", err)
	}

	controllers := buildControllerIndex(vmMo.Config.Hardware.Device)

	var disks []DiskInfo
	for _, dev := range vmMo.Config.Hardware.Device {
		disk, ok := dev.(*types.VirtualDisk)
		if !ok {
			continue
		}
		limit := int64(-1)
		if disk.StorageIOAllocation != nil && disk.StorageIOAllocation.Limit != nil {
			limit = *disk.StorageIOAllocation.Limit
		}
		addr, ctrlName := diskAddress(controllers, disk)
		disks = append(disks, DiskInfo{
			Key:         disk.Key,
			Label:       deviceLabel(disk),
			SizeGB:      float64(disk.CapacityInKB) / 1024.0 / 1024.0,
			Limit:       limit,
			SCSIAddress: addr,
			Controller:  ctrlName,
		})
	}
	sort.Slice(disks, func(i, j int) bool { return disks[i].Key < disks[j].Key })
	return disks, nil
}

// deviceLabel возвращает отображаемое имя диска (например "Hard disk 1"),
// как оно показано в vSphere Client.
//
// deviceLabel returns the disk's display name (e.g. "Hard disk 1"), matching
// what vSphere Client shows.
func deviceLabel(disk *types.VirtualDisk) string {
	if disk.DeviceInfo != nil {
		info := disk.DeviceInfo.GetDescription()
		if info != nil && info.Label != "" {
			return info.Label
		}
	}
	return fmt.Sprintf("Disk (key %d)", disk.Key)
}

// buildControllerIndex индексирует контроллеры ВМ (SCSI/SATA/NVMe/IDE) по их
// device key, чтобы быстро находить контроллер диска по disk.ControllerKey.
//
// buildControllerIndex indexes a VM's controllers (SCSI/SATA/NVMe/IDE) by
// device key, for quick lookup of a disk's controller via disk.ControllerKey.
func buildControllerIndex(devices []types.BaseVirtualDevice) map[int32]types.BaseVirtualDevice {
	idx := make(map[int32]types.BaseVirtualDevice)
	for _, d := range devices {
		if _, ok := d.(types.BaseVirtualController); !ok {
			continue
		}
		gd := d.GetVirtualDevice()
		if gd == nil {
			continue
		}
		idx[gd.Key] = d
	}
	return idx
}

// controllerTypeName возвращает человекочитаемое имя типа контроллера —
// то же, что видно в UI vSphere при выборе диска (LSI Logic SAS, VMware
// Paravirtual и т.д.).
//
// controllerTypeName returns the controller type's human-readable name,
// matching what vSphere UI shows when a disk is selected (LSI Logic SAS,
// VMware Paravirtual, etc.).
func controllerTypeName(d types.BaseVirtualDevice) string {
	switch d.(type) {
	case *types.ParaVirtualSCSIController:
		return "VMware Paravirtual"
	case *types.VirtualLsiLogicSASController:
		return "LSI Logic SAS"
	case *types.VirtualLsiLogicController:
		return "LSI Logic Parallel"
	case *types.VirtualBusLogicController:
		return "BusLogic Parallel"
	case *types.VirtualIDEController:
		return "IDE"
	case *types.VirtualAHCIController:
		return "SATA"
	case *types.VirtualNVMEController:
		return "NVMe"
	default:
		return ""
	}
}

// diskAddress возвращает SCSI/SATA/NVMe-адрес диска в виде "bus:unit" (как в
// vSphere Client, например "0:0") и имя типа контроллера, используя заранее
// построенный индекс контроллеров ВМ. Если контроллер не найден, возвращает
// пустые строки.
//
// diskAddress returns the disk's SCSI/SATA/NVMe address as "bus:unit" (as
// shown in vSphere Client, e.g. "0:0") and the controller type name, using a
// pre-built controller index. Returns empty strings if the controller isn't found.
func diskAddress(controllers map[int32]types.BaseVirtualDevice, disk *types.VirtualDisk) (address string, controller string) {
	ctrlDevice, ok := controllers[disk.ControllerKey]
	if !ok {
		return "", ""
	}
	bc, ok := ctrlDevice.(types.BaseVirtualController)
	if !ok {
		return "", ""
	}
	vc := bc.GetVirtualController()
	if vc == nil {
		return "", ""
	}
	unit := int32(0)
	if disk.UnitNumber != nil {
		unit = *disk.UnitNumber
	}
	address = fmt.Sprintf("%d:%d", vc.BusNumber, unit)
	controller = controllerTypeName(ctrlDevice)
	return address, controller
}

// SetDiskLimit меняет лимит IOPS для одного диска ВМ через ReconfigVM_Task,
// передавая полный объект диска (а не частичный diff).
//
// SetDiskLimit changes the IOPS limit of a single VM disk via
// ReconfigVM_Task, submitting the full disk device object (not a partial diff).
func (m *Manager) SetDiskLimit(ctx context.Context, conn *Connection, vmRef string, diskKey int32, limit int64) error {
	vm := vmObject(conn, vmRef)

	var vmMo mo.VirtualMachine
	err := vm.Properties(ctx, vm.Reference(), []string{"name", "config.hardware.device"}, &vmMo)
	if err != nil {
		return fmt.Errorf("не удалось получить конфигурацию ВМ: %w", err)
	}

	var target *types.VirtualDisk
	for _, dev := range vmMo.Config.Hardware.Device {
		if disk, ok := dev.(*types.VirtualDisk); ok && disk.Key == diskKey {
			target = disk
			break
		}
	}
	if target == nil {
		return fmt.Errorf("диск с ключом %d не найден", diskKey)
	}

	oldLimit := int64(-1)
	if target.StorageIOAllocation != nil && target.StorageIOAllocation.Limit != nil {
		oldLimit = *target.StorageIOAllocation.Limit
	}

	if target.StorageIOAllocation == nil {
		target.StorageIOAllocation = &types.StorageIOAllocationInfo{}
	}
	lim := limit
	target.StorageIOAllocation.Limit = &lim

	spec := types.VirtualMachineConfigSpec{
		DeviceChange: []types.BaseVirtualDeviceConfigSpec{
			&types.VirtualDeviceConfigSpec{
				Operation: types.VirtualDeviceConfigSpecOperationEdit,
				Device:    target,
			},
		},
	}

	task, err := vm.Reconfigure(ctx, spec)
	if err != nil {
		return fmt.Errorf("ошибка запуска Reconfigure: %w", err)
	}
	if err := task.Wait(ctx); err != nil {
		return err
	}

	logAction(conn, "лимит IOPS: VM=%q (%s) диск %q (key %d): %s -> %s",
		vmMo.Name, vmRef, deviceLabel(target), diskKey, limitStr(oldLimit), limitStr(limit))
	return nil
}

// limitStr форматирует значение лимита для записи в журнал ("Unlimited" для -1).
//
// limitStr formats a limit value for the log ("Unlimited" for -1).
func limitStr(v int64) string {
	if v == -1 {
		return "Unlimited"
	}
	return strconv.FormatInt(v, 10)
}

// ResetAllLimits сбрасывает лимит IOPS (в Unlimited, -1) на всех дисках ВМ одним вызовом.
//
// ResetAllLimits resets the IOPS limit (to Unlimited, -1) on all of a VM's
// disks in one call.
func (m *Manager) ResetAllLimits(ctx context.Context, conn *Connection, vmRef string) (int, error) {
	return m.resetLimitsOnVM(ctx, conn, vmRef, nil, "")
}

// resetLimitsOnVM сбрасывает лимит IOPS в Unlimited (-1) на дисках ВМ одним
// вызовом Reconfigure. Если onlyKeys не nil, меняются только диски с ключом
// из этого набора (используется при сбросе по датастору/resource pool,
// когда нужны не все диски ВМ). scopeLabel используется только для журнала
// (пустая строка — обычный сброс всех дисков ВМ).
//
// resetLimitsOnVM resets the IOPS limit to Unlimited (-1) on a VM's disks in
// one Reconfigure call. If onlyKeys is non-nil, only disks whose key is in
// that set are changed (used by datastore/resource-pool scoped resets, which
// target a subset of a VM's disks). scopeLabel is used only for logging
// (an empty string means a plain reset-all-disks call).
func (m *Manager) resetLimitsOnVM(ctx context.Context, conn *Connection, vmRef string, onlyKeys map[int32]bool, scopeLabel string) (int, error) {
	vm := vmObject(conn, vmRef)

	var vmMo mo.VirtualMachine
	err := vm.Properties(ctx, vm.Reference(), []string{"name", "config.hardware.device"}, &vmMo)
	if err != nil {
		return 0, fmt.Errorf("не удалось получить конфигурацию ВМ: %w", err)
	}

	var specs []types.BaseVirtualDeviceConfigSpec
	var changedLabels []string
	unlimited := int64(-1)
	for _, dev := range vmMo.Config.Hardware.Device {
		disk, ok := dev.(*types.VirtualDisk)
		if !ok {
			continue
		}
		if onlyKeys != nil && !onlyKeys[disk.Key] {
			continue
		}
		if disk.StorageIOAllocation == nil {
			disk.StorageIOAllocation = &types.StorageIOAllocationInfo{}
		}
		lim := unlimited
		disk.StorageIOAllocation.Limit = &lim
		specs = append(specs, &types.VirtualDeviceConfigSpec{
			Operation: types.VirtualDeviceConfigSpecOperationEdit,
			Device:    disk,
		})
		changedLabels = append(changedLabels, deviceLabel(disk))
	}

	if len(specs) == 0 {
		return 0, nil
	}

	spec := types.VirtualMachineConfigSpec{DeviceChange: specs}
	task, err := vm.Reconfigure(ctx, spec)
	if err != nil {
		return 0, fmt.Errorf("ошибка запуска Reconfigure: %w", err)
	}
	if err := task.Wait(ctx); err != nil {
		return 0, err
	}

	if scopeLabel != "" {
		logAction(conn, "сброс лимитов [%s]: VM=%q (%s) диски: %s -> Unlimited", scopeLabel, vmMo.Name, vmRef, strings.Join(changedLabels, ", "))
	} else {
		logAction(conn, "сброс всех лимитов: VM=%q (%s) диски: %s -> Unlimited", vmMo.Name, vmRef, strings.Join(changedLabels, ", "))
	}
	return len(specs), nil
}

// ResourcePoolInfo — то, что отдаётся во фронтенд при поиске resource pool.
//
// ResourcePoolInfo is sent to the frontend from a resource pool search.
type ResourcePoolInfo struct {
	Ref   string `json:"ref"`
	Name  string `json:"name"`
	Owner string `json:"owner"` // имя кластера/хоста-владельца, для разграничения одноимённых пулов / owning cluster/host name, to tell same-named pools apart
}

// PoolResetError — ошибка сброса лимитов на одной ВМ внутри resource pool/датастора.
//
// PoolResetError is a reset error for one VM inside a resource pool/datastore batch.
type PoolResetError struct {
	VM    string `json:"vm"`
	Error string `json:"error"`
}

// PoolResetResult — сводный результат массового сброса лимитов по resource
// pool или датастору.
//
// PoolResetResult is the summary of a bulk limit reset over a resource pool
// or a datastore.
type PoolResetResult struct {
	VMs        int              `json:"vms"`
	DisksReset int              `json:"disksReset"`
	Errors     []PoolResetError `json:"errors"`
}

// ownerName возвращает имя ComputeResource (кластера/хоста), которому
// принадлежит resource pool.
//
// ownerName returns the name of the ComputeResource (cluster/host) that owns
// a resource pool.
func ownerName(ctx context.Context, client *vim25.Client, ref types.ManagedObjectReference) string {
	if ref.Value == "" {
		return ""
	}
	var ce mo.ComputeResource
	if err := mo.RetrieveProperties(ctx, client, client.ServiceContent.PropertyCollector, ref, &ce); err != nil {
		return ""
	}
	return ce.Name
}

// SearchResourcePools ищет resource pool по подстроке имени (без учёта регистра).
//
// SearchResourcePools finds resource pools by a case-insensitive name substring.
func (m *Manager) SearchResourcePools(ctx context.Context, conn *Connection, query string) ([]ResourcePoolInfo, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("пустой поисковый запрос")
	}

	client := conn.Client.Client
	vw := view.NewManager(client)
	cv, err := vw.CreateContainerView(ctx, client.ServiceContent.RootFolder, []string{"ResourcePool"}, true)
	if err != nil {
		return nil, err
	}
	defer cv.Destroy(ctx)

	var pools []mo.ResourcePool
	if err := cv.Retrieve(ctx, []string{"ResourcePool"}, []string{"name", "owner"}, &pools); err != nil {
		return nil, err
	}

	lowerQuery := strings.ToLower(query)
	ownerNames := make(map[string]string)
	var results []ResourcePoolInfo
	for _, p := range pools {
		if !strings.Contains(strings.ToLower(p.Name), lowerQuery) {
			continue
		}
		ownerKey := p.Owner.Type + ":" + p.Owner.Value
		name, ok := ownerNames[ownerKey]
		if !ok {
			name = ownerName(ctx, client, p.Owner)
			ownerNames[ownerKey] = name
		}
		results = append(results, ResourcePoolInfo{Ref: p.Self.Value, Name: p.Name, Owner: name})
	}
	sort.Slice(results, func(i, j int) bool { return results[i].Name < results[j].Name })

	if len(results) > 200 {
		results = results[:200]
	}
	return results, nil
}

// ResourcePoolVMs возвращает ВМ, находящиеся непосредственно в указанном
// resource pool — через его собственное свойство "vm" (прямые дети). ВМ во
// вложенных resource pool в результат не включаются.
//
// ResourcePoolVMs returns the VMs that sit directly inside the given
// resource pool, via its own "vm" property (direct children only). VMs in
// nested resource pools are excluded from the result.
func (m *Manager) ResourcePoolVMs(ctx context.Context, conn *Connection, poolRef string) ([]VMSummary, error) {
	client := conn.Client.Client
	ref := types.ManagedObjectReference{Type: "ResourcePool", Value: poolRef}

	rp := object.NewResourcePool(client, ref)
	var poolMo mo.ResourcePool
	if err := rp.Properties(ctx, ref, []string{"vm"}, &poolMo); err != nil {
		return nil, fmt.Errorf("не удалось получить список ВМ resource pool: %w", err)
	}

	if len(poolMo.Vm) == 0 {
		return []VMSummary{}, nil
	}

	type fetchResult struct {
		vm  VMSummary
		err error
	}
	const concurrency = 8
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	resultsCh := make(chan fetchResult, len(poolMo.Vm))

	for _, vmRef := range poolMo.Vm {
		wg.Add(1)
		sem <- struct{}{}
		go func(vmRef types.ManagedObjectReference) {
			defer wg.Done()
			defer func() { <-sem }()

			vmObj := object.NewVirtualMachine(client, vmRef)
			var vmMo mo.VirtualMachine
			if err := vmObj.Properties(ctx, vmRef, []string{"name", "runtime.powerState"}, &vmMo); err != nil {
				resultsCh <- fetchResult{err: err}
				return
			}
			resultsCh <- fetchResult{vm: VMSummary{Ref: vmRef.Value, Name: vmMo.Name, PowerState: string(vmMo.Runtime.PowerState)}}
		}(vmRef)
	}

	go func() {
		wg.Wait()
		close(resultsCh)
	}()

	results := make([]VMSummary, 0, len(poolMo.Vm))
	for r := range resultsCh {
		if r.err != nil {
			continue // ВМ стала недоступна между чтением списка и опросом / VM became unreachable between listing and querying
		}
		results = append(results, r.vm)
	}
	sort.Slice(results, func(i, j int) bool { return results[i].Name < results[j].Name })
	return results, nil
}

// ResetPoolLimits сбрасывает лимиты IOPS на всех дисках ВМ, находящихся
// непосредственно в resource pool (см. ResourcePoolVMs). Каждая ВМ
// сбрасывается отдельным вызовом Reconfigure с ограниченным параллелизмом.
//
// ResetPoolLimits resets IOPS limits on all disks of the VMs directly inside
// a resource pool (see ResourcePoolVMs). Each VM is reset with a separate
// Reconfigure call, with limited concurrency.
func (m *Manager) ResetPoolLimits(ctx context.Context, conn *Connection, poolRef string) (*PoolResetResult, error) {
	pool, err := m.resourcePoolByRef(ctx, conn, poolRef)
	if err != nil {
		return nil, err
	}
	vms, err := m.ResourcePoolVMs(ctx, conn, poolRef)
	if err != nil {
		return nil, err
	}

	scope := fmt.Sprintf("resource pool %q", pool)
	result := &PoolResetResult{}
	const concurrency = 3
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex

	for _, vm := range vms {
		wg.Add(1)
		sem <- struct{}{}
		go func(vm VMSummary) {
			defer wg.Done()
			defer func() { <-sem }()

			count, err := m.resetLimitsOnVM(ctx, conn, vm.Ref, nil, scope)

			mu.Lock()
			defer mu.Unlock()
			result.VMs++
			if err != nil {
				result.Errors = append(result.Errors, PoolResetError{VM: vm.Name, Error: err.Error()})
				return
			}
			result.DisksReset += count
		}(vm)
	}
	wg.Wait()

	sort.Slice(result.Errors, func(i, j int) bool { return result.Errors[i].VM < result.Errors[j].VM })
	logAction(conn, "сводка сброса по resource pool %q: ВМ обработано %d, дисков сброшено %d, ошибок %d",
		pool, result.VMs, result.DisksReset, len(result.Errors))
	return result, nil
}

// resourcePoolByRef возвращает имя resource pool по его moref, для записи в
// журнал; при ошибке возвращает сам ref как запасной вариант.
//
// resourcePoolByRef returns a resource pool's name by its moref, for logging;
// on error it falls back to returning the ref itself.
func (m *Manager) resourcePoolByRef(ctx context.Context, conn *Connection, poolRef string) (string, error) {
	ref := types.ManagedObjectReference{Type: "ResourcePool", Value: poolRef}
	var p mo.ResourcePool
	if err := mo.RetrieveProperties(ctx, conn.Client.Client, conn.Client.Client.ServiceContent.PropertyCollector, ref, &p); err != nil {
		return poolRef, nil
	}
	return p.Name, nil
}

// DatastoreInfo — то, что отдаётся во фронтенд при поиске датасторов.
//
// DatastoreInfo is sent to the frontend from a datastore search.
type DatastoreInfo struct {
	Ref  string `json:"ref"`
	Name string `json:"name"`
}

// DatastoreVMDisks — ВМ и те её диски, что лежат на конкретном датасторе
// (другие диски той же ВМ на других датасторах сюда не входят).
//
// DatastoreVMDisks is a VM together with only the disks it has on one
// specific datastore (the same VM's disks on other datastores are excluded).
type DatastoreVMDisks struct {
	VMRef      string     `json:"vmRef"`
	VMName     string     `json:"vmName"`
	PowerState string     `json:"powerState"`
	Disks      []DiskInfo `json:"disks"`
}

// SearchDatastores ищет датасторы по подстроке имени (без учёта регистра).
//
// SearchDatastores finds datastores by a case-insensitive name substring.
func (m *Manager) SearchDatastores(ctx context.Context, conn *Connection, query string) ([]DatastoreInfo, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("пустой поисковый запрос")
	}

	client := conn.Client.Client
	vw := view.NewManager(client)
	cv, err := vw.CreateContainerView(ctx, client.ServiceContent.RootFolder, []string{"Datastore"}, true)
	if err != nil {
		return nil, err
	}
	defer cv.Destroy(ctx)

	var stores []mo.Datastore
	if err := cv.Retrieve(ctx, []string{"Datastore"}, []string{"name"}, &stores); err != nil {
		return nil, err
	}

	lowerQuery := strings.ToLower(query)
	var results []DatastoreInfo
	for _, ds := range stores {
		if strings.Contains(strings.ToLower(ds.Name), lowerQuery) {
			results = append(results, DatastoreInfo{Ref: ds.Self.Value, Name: ds.Name})
		}
	}
	sort.Slice(results, func(i, j int) bool { return results[i].Name < results[j].Name })

	if len(results) > 200 {
		results = results[:200]
	}
	return results, nil
}

// diskDatastoreRef возвращает moref датастора, на котором лежит файл диска,
// либо nil, если бэкинг диска не файловый (например, RDM passthrough).
//
// diskDatastoreRef returns the moref of the datastore holding the disk's
// file, or nil if the disk's backing isn't file-based (e.g. RDM passthrough).
func diskDatastoreRef(disk *types.VirtualDisk) *types.ManagedObjectReference {
	fileBacking, ok := disk.Backing.(types.BaseVirtualDeviceFileBackingInfo)
	if !ok {
		return nil
	}
	info := fileBacking.GetVirtualDeviceFileBackingInfo()
	if info == nil {
		return nil
	}
	return info.Datastore
}

// DatastoreVMs возвращает все ВМ, у которых есть хотя бы один диск на
// указанном датасторе, вместе со списком именно тех дисков.
//
// DatastoreVMs returns every VM that has at least one disk on the given
// datastore, together with just those disks.
func (m *Manager) DatastoreVMs(ctx context.Context, conn *Connection, dsRef string) ([]DatastoreVMDisks, error) {
	client := conn.Client.Client
	dsMoRef := types.ManagedObjectReference{Type: "Datastore", Value: dsRef}

	ds := object.NewDatastore(client, dsMoRef)
	var dsMo mo.Datastore
	if err := ds.Properties(ctx, dsMoRef, []string{"vm"}, &dsMo); err != nil {
		return nil, fmt.Errorf("не удалось получить список ВМ датастора: %w", err)
	}

	type fetchResult struct {
		info DatastoreVMDisks
		ok   bool
		err  error
	}

	const concurrency = 8
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	resultsCh := make(chan fetchResult, len(dsMo.Vm))

	for _, vmRef := range dsMo.Vm {
		wg.Add(1)
		sem <- struct{}{}
		go func(vmRef types.ManagedObjectReference) {
			defer wg.Done()
			defer func() { <-sem }()

			vmObj := object.NewVirtualMachine(client, vmRef)
			var vmMo mo.VirtualMachine
			if err := vmObj.Properties(ctx, vmRef, []string{"name", "runtime.powerState", "config.hardware.device"}, &vmMo); err != nil {
				resultsCh <- fetchResult{err: err}
				return
			}

			controllers := buildControllerIndex(vmMo.Config.Hardware.Device)

			var disks []DiskInfo
			for _, dev := range vmMo.Config.Hardware.Device {
				disk, ok := dev.(*types.VirtualDisk)
				if !ok {
					continue
				}
				dsForDisk := diskDatastoreRef(disk)
				if dsForDisk == nil || dsForDisk.Value != dsRef {
					continue
				}
				limit := int64(-1)
				if disk.StorageIOAllocation != nil && disk.StorageIOAllocation.Limit != nil {
					limit = *disk.StorageIOAllocation.Limit
				}
				addr, ctrlName := diskAddress(controllers, disk)
				disks = append(disks, DiskInfo{
					Key:         disk.Key,
					Label:       deviceLabel(disk),
					SizeGB:      float64(disk.CapacityInKB) / 1024.0 / 1024.0,
					Limit:       limit,
					SCSIAddress: addr,
					Controller:  ctrlName,
				})
			}
			if len(disks) == 0 {
				resultsCh <- fetchResult{ok: false}
				return
			}
			sort.Slice(disks, func(i, j int) bool { return disks[i].Key < disks[j].Key })
			resultsCh <- fetchResult{ok: true, info: DatastoreVMDisks{
				VMRef:      vmRef.Value,
				VMName:     vmMo.Name,
				PowerState: string(vmMo.Runtime.PowerState),
				Disks:      disks,
			}}
		}(vmRef)
	}

	go func() {
		wg.Wait()
		close(resultsCh)
	}()

	var results []DatastoreVMDisks
	for r := range resultsCh {
		if r.err != nil {
			continue // ВМ стала недоступна между запросом списка и опросом / VM became unreachable between listing and querying
		}
		if r.ok {
			results = append(results, r.info)
		}
	}
	sort.Slice(results, func(i, j int) bool { return results[i].VMName < results[j].VMName })
	return results, nil
}

// ResetDatastoreLimits сбрасывает лимиты IOPS только на тех дисках, что
// лежат на указанном датасторе — остальные диски затронутых ВМ не трогает.
//
// ResetDatastoreLimits resets IOPS limits only on the disks that live on the
// given datastore, leaving the affected VMs' other disks untouched.
func (m *Manager) ResetDatastoreLimits(ctx context.Context, conn *Connection, dsRef string) (*PoolResetResult, error) {
	dsName, err := m.datastoreNameByRef(ctx, conn, dsRef)
	if err != nil {
		dsName = dsRef
	}
	vms, err := m.DatastoreVMs(ctx, conn, dsRef)
	if err != nil {
		return nil, err
	}

	scope := fmt.Sprintf("датастор %q", dsName)
	result := &PoolResetResult{}
	const concurrency = 3
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex

	for _, vm := range vms {
		keys := make(map[int32]bool, len(vm.Disks))
		for _, d := range vm.Disks {
			keys[d.Key] = true
		}

		wg.Add(1)
		sem <- struct{}{}
		go func(vm DatastoreVMDisks, keys map[int32]bool) {
			defer wg.Done()
			defer func() { <-sem }()

			count, err := m.resetLimitsOnVM(ctx, conn, vm.VMRef, keys, scope)

			mu.Lock()
			defer mu.Unlock()
			result.VMs++
			if err != nil {
				result.Errors = append(result.Errors, PoolResetError{VM: vm.VMName, Error: err.Error()})
				return
			}
			result.DisksReset += count
		}(vm, keys)
	}
	wg.Wait()

	sort.Slice(result.Errors, func(i, j int) bool { return result.Errors[i].VM < result.Errors[j].VM })
	logAction(conn, "сводка сброса по датастору %q: ВМ обработано %d, дисков сброшено %d, ошибок %d",
		dsName, result.VMs, result.DisksReset, len(result.Errors))
	return result, nil
}

// datastoreNameByRef возвращает имя датастора по его moref, для записи в журнал.
//
// datastoreNameByRef returns a datastore's name by its moref, for logging.
func (m *Manager) datastoreNameByRef(ctx context.Context, conn *Connection, dsRef string) (string, error) {
	ref := types.ManagedObjectReference{Type: "Datastore", Value: dsRef}
	var ds mo.Datastore
	if err := mo.RetrieveProperties(ctx, conn.Client.Client, conn.Client.Client.ServiceContent.PropertyCollector, ref, &ds); err != nil {
		return "", err
	}
	return ds.Name, nil
}

// PathCheckResult — результат проверки одного пути (.vmx или .vmdk): найден
// ли его датастор, существует ли сам файл и зарегистрирован ли он за какой-либо ВМ.
//
// PathCheckResult is the outcome of checking one .vmx/.vmdk path: whether its
// datastore was found, whether the file itself exists, and whether it is
// registered to any VM.
type PathCheckResult struct {
	Path           string   `json:"path"`
	Kind           string   `json:"kind"`           // "vmx" | "vmdk" | "unknown"
	DatastoreFound bool     `json:"datastoreFound"` // датастор из "[имя]" существует в этом vCenter / the "[name]" datastore exists in this vCenter
	FileExists     bool     `json:"fileExists"`     // файл физически найден на датасторе (значим только если DatastoreFound и Kind != unknown) / file physically found on the datastore (meaningful only if DatastoreFound and Kind != unknown)
	Zombie         bool     `json:"zombie"`         // файл существует физически, но не числится ни за одной ВМ / file physically exists but isn't registered to any VM
	VMs            []string `json:"vms"`            // ВМ, за которыми числится этот файл (обычно 0 или 1) / VMs this file is registered to (usually 0 or 1)

	dsRef types.ManagedObjectReference // ref датастора, для проверки физического наличия файла / datastore ref, used to check physical file existence
}

// normalizeDatastorePath приводит путь датастора к единому виду для
// сравнения: обрезает пробелы по краям, схлопывает повторные пробелы и
// переводит в нижний регистр (сравнение регистронезависимое, как -eq в PowerShell).
//
// normalizeDatastorePath normalizes a datastore path for comparison:
// trims outer whitespace, collapses repeated spaces, and lowercases it
// (comparison is case-insensitive, matching PowerShell's -eq).
func normalizeDatastorePath(p string) string {
	p = strings.TrimSpace(p)
	p = strings.Join(strings.Fields(p), " ")
	return strings.ToLower(p)
}

// extractDatastoreName достаёт имя датастора из пути вида "[имя] папка/файл".
//
// extractDatastoreName extracts the datastore name from a path like
// "[name] folder/file".
func extractDatastoreName(p string) (string, bool) {
	p = strings.TrimSpace(p)
	if !strings.HasPrefix(p, "[") {
		return "", false
	}
	idx := strings.Index(p, "]")
	if idx < 0 {
		return "", false
	}
	return strings.TrimSpace(p[1:idx]), true
}

// splitDatastoreRelativePath разбирает путь вида "[имя] папка/подпапка/файл.ext"
// на папку ("папка/подпапка", может быть пустой для файла в корне датастора)
// и имя файла ("файл.ext").
//
// splitDatastoreRelativePath splits a path like "[name] folder/subfolder/file.ext"
// into a folder ("folder/subfolder", empty for a file at the datastore root)
// and a filename ("file.ext").
func splitDatastoreRelativePath(p string) (folder string, filename string, ok bool) {
	p = strings.TrimSpace(p)
	idx := strings.Index(p, "]")
	if !strings.HasPrefix(p, "[") || idx < 0 {
		return "", "", false
	}
	rest := strings.TrimSpace(p[idx+1:])
	if rest == "" {
		return "", "", false
	}
	slash := strings.LastIndex(rest, "/")
	if slash < 0 {
		return "", rest, true
	}
	return rest[:slash], rest[slash+1:], true
}

// CheckPaths проверяет список путей вида "[Datastore] folder/file.vmx" или
// "...vmdk": существует ли их датастор в этом vCenter, существует ли сам файл
// физически на датасторе, и зарегистрирован ли он хотя бы за одной ВМ —
// эквивалент ручной проверки через
//
//	Get-VM | Get-HardDisk | Where-Object { $_.Filename -eq "..." }
//	Get-VM | Where-Object { $_.ExtensionData.Config.Files.VmPathName -eq "..." }
//
// Сравнение регистронезависимое, как оператор -eq в PowerShell. Индекс по
// всем ВМ строится один раз, а затем по нему проверяются все переданные пути
// (без отдельного сканирования ВМ на каждый путь). Физическое наличие файла
// на датасторе отдельно проверяется через HostDatastoreBrowser.
//
// Для vmdk учитывается только текущий (верхний) файл в цепочке снапшота —
// как $_.Filename у Get-HardDisk без обхода цепочки delta-файлов.
//
// CheckPaths checks a list of paths like "[Datastore] folder/file.vmx" or
// "...vmdk": whether their datastore exists in this vCenter, whether the
// file itself physically exists on the datastore, and whether it's
// registered to at least one VM — equivalent to the manual check via
//
//	Get-VM | Get-HardDisk | Where-Object { $_.Filename -eq "..." }
//	Get-VM | Where-Object { $_.ExtensionData.Config.Files.VmPathName -eq "..." }
//
// Comparison is case-insensitive, matching PowerShell's -eq. A VM index is
// built once, then all submitted paths are checked against it (no per-path
// VM scan). Physical file existence on the datastore is checked separately
// via HostDatastoreBrowser.
//
// For vmdk, only the current (topmost) file in the snapshot chain is
// considered — matching Get-HardDisk's $_.Filename without walking the
// delta-file chain.
func (m *Manager) CheckPaths(ctx context.Context, conn *Connection, paths []string) ([]PathCheckResult, error) {
	client := conn.Client.Client

	dsView, err := view.NewManager(client).CreateContainerView(ctx, client.ServiceContent.RootFolder, []string{"Datastore"}, true)
	if err != nil {
		return nil, err
	}
	var stores []mo.Datastore
	err = dsView.Retrieve(ctx, []string{"Datastore"}, []string{"name"}, &stores)
	dsView.Destroy(ctx)
	if err != nil {
		return nil, err
	}
	knownDatastores := make(map[string]bool, len(stores))
	datastoreRefs := make(map[string]types.ManagedObjectReference, len(stores))
	for _, ds := range stores {
		key := strings.ToLower(ds.Name)
		knownDatastores[key] = true
		datastoreRefs[key] = ds.Self
	}

	vw := view.NewManager(client)
	cv, err := vw.CreateContainerView(ctx, client.ServiceContent.RootFolder, []string{"VirtualMachine"}, true)
	if err != nil {
		return nil, err
	}
	defer cv.Destroy(ctx)

	var vms []mo.VirtualMachine
	if err := cv.Retrieve(ctx, []string{"VirtualMachine"}, []string{"name", "config.files", "config.hardware.device"}, &vms); err != nil {
		return nil, err
	}

	vmxIndex := make(map[string][]string)
	vmdkIndex := make(map[string][]string)
	for _, v := range vms {
		if v.Config == nil {
			continue // ВМ в промежуточном состоянии, без конфигурации / VM in a transient state, no config yet
		}
		if v.Config.Files.VmPathName != "" {
			key := normalizeDatastorePath(v.Config.Files.VmPathName)
			vmxIndex[key] = append(vmxIndex[key], v.Name)
		}
		for _, dev := range v.Config.Hardware.Device {
			disk, ok := dev.(*types.VirtualDisk)
			if !ok {
				continue
			}
			fileBacking, ok := disk.Backing.(types.BaseVirtualDeviceFileBackingInfo)
			if !ok {
				continue
			}
			info := fileBacking.GetVirtualDeviceFileBackingInfo()
			if info == nil || info.FileName == "" {
				continue
			}
			key := normalizeDatastorePath(info.FileName)
			vmdkIndex[key] = append(vmdkIndex[key], v.Name)
		}
	}

	results := make([]PathCheckResult, 0, len(paths))
	for _, raw := range paths {
		p := strings.TrimSpace(raw)
		if p == "" {
			continue
		}
		lower := strings.ToLower(p)
		key := normalizeDatastorePath(p)

		dsFound := false
		var dsRef types.ManagedObjectReference
		if dsName, ok := extractDatastoreName(p); ok {
			dsKey := strings.ToLower(dsName)
			dsFound = knownDatastores[dsKey]
			if dsFound {
				dsRef = datastoreRefs[dsKey]
			}
		}

		var kind string
		var matches []string
		switch {
		case strings.HasSuffix(lower, ".vmx"):
			kind = "vmx"
			matches = vmxIndex[key]
		case strings.HasSuffix(lower, ".vmdk"):
			kind = "vmdk"
			matches = vmdkIndex[key]
		default:
			kind = "unknown"
		}

		results = append(results, PathCheckResult{
			Path:           p,
			Kind:           kind,
			DatastoreFound: dsFound,
			VMs:            matches,
			dsRef:          dsRef,
		})
	}

	// Для путей, где датастор найден и тип файла распознан, проверяем
	// физическое наличие файла на датасторе через HostDatastoreBrowser.
	//
	// For paths with a known datastore and recognized file type, check the
	// file's physical existence on the datastore via HostDatastoreBrowser.
	browsers := make(map[string]*object.HostDatastoreBrowser)
	var browsersMu sync.Mutex
	getBrowser := func(ref types.ManagedObjectReference) (*object.HostDatastoreBrowser, error) {
		browsersMu.Lock()
		if b, ok := browsers[ref.Value]; ok {
			browsersMu.Unlock()
			return b, nil
		}
		browsersMu.Unlock()

		ds := object.NewDatastore(client, ref)
		b, err := ds.Browser(ctx)
		if err != nil {
			return nil, err
		}

		browsersMu.Lock()
		browsers[ref.Value] = b
		browsersMu.Unlock()
		return b, nil
	}

	const checkConcurrency = 8
	sem := make(chan struct{}, checkConcurrency)
	var wg sync.WaitGroup

	for i := range results {
		r := &results[i]
		if r.Kind == "unknown" || !r.DatastoreFound {
			continue
		}
		folder, filename, ok := splitDatastoreRelativePath(r.Path)
		if !ok || filename == "" {
			continue
		}

		wg.Add(1)
		sem <- struct{}{}
		go func(r *PathCheckResult, folder, filename string) {
			defer wg.Done()
			defer func() { <-sem }()

			browser, err := getBrowser(r.dsRef)
			if err != nil {
				// FileExists остаётся false; ошибка одного пути не прерывает всю проверку.
				// FileExists stays false; one path's error doesn't abort the whole check.
				return
			}

			datastorePath := object.DatastorePath{Datastore: datastoreNameFromRef(stores, r.dsRef), Path: folder}
			searchSpec := &types.HostDatastoreBrowserSearchSpec{
				MatchPattern: []string{filename},
			}
			task, err := browser.SearchDatastore(ctx, datastorePath.String(), searchSpec)
			if err != nil {
				return
			}
			info, err := task.WaitForResult(ctx, nil)
			if err != nil {
				// Например, папка не существует — трактуется как "файла нет".
				// E.g. the folder doesn't exist — treated as "file not found".
				return
			}
			res, ok := info.Result.(types.HostDatastoreBrowserSearchResults)
			if !ok {
				return
			}
			r.FileExists = len(res.File) > 0
		}(r, folder, filename)
	}
	wg.Wait()

	notFound := 0
	zombies := 0
	for i := range results {
		r := &results[i]
		if r.Kind == "unknown" || !r.DatastoreFound {
			continue
		}
		if !r.FileExists {
			notFound++
			continue
		}
		r.Zombie = len(r.VMs) == 0
		if r.Zombie {
			zombies++
		}
	}
	logAction(conn, "проверка файлов на zombie: проверено %d, файлов не найдено на датасторе %d, найдено zombie %d", len(results), notFound, zombies)

	return results, nil
}

// datastoreNameFromRef ищет имя датастора по его ref среди уже загруженного списка.
//
// datastoreNameFromRef looks up a datastore's name by ref within an
// already-fetched list.
func datastoreNameFromRef(stores []mo.Datastore, ref types.ManagedObjectReference) string {
	for _, ds := range stores {
		if ds.Self == ref {
			return ds.Name
		}
	}
	return ""
}

// ---- вспомогательное / helpers ----

// parseFinderFallback ищет ВМ по имени/пути через find.Finder (запасной способ поиска).
//
// parseFinderFallback finds a VM by name/path via find.Finder (a fallback lookup).
func parseFinderFallback(ctx context.Context, client *govmomi.Client, name string) (*object.VirtualMachine, error) {
	f := find.NewFinder(client.Client, true)
	return f.VirtualMachine(ctx, name)
}

// atoi64 разбирает строку в int64, обрезая пробелы по краям.
//
// atoi64 parses a string into an int64, trimming outer whitespace.
func atoi64(s string) (int64, error) {
	return strconv.ParseInt(strings.TrimSpace(s), 10, 64)
}
