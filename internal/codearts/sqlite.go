package codearts

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// 极简 SQLite 只读 / 定长原地写解析器。
//
// 只服务一个用途：读取（以及在长度不变时原地覆写）VS Code secret storage
// 里 `ItemTable` 的一行。刻意不引入任何第三方依赖。
//
// 写路径刻意只做「同长度字节覆写」——不新增/删除 cell、不改页结构、
// 不动 freelist，因此不存在把数据库改坏的结构性风险。长度不一致时调用方
// 应放弃写入（见 session_windows.go）。

type sqliteDB struct {
	data     []byte
	pageSize int
	usable   int
}

// sqlValue 是 SQLite 记录中的一个值。
type sqlValue struct {
	Type  int // 0=null 1=int 2=float 3=text 4=blob
	Int   int64
	Float float64
	Text  string
	Blob  []byte
}

// AsString 以字符串形式返回该值。
func (v sqlValue) AsString() string {
	switch v.Type {
	case 3:
		return v.Text
	case 4:
		return string(v.Blob)
	case 1:
		return fmt.Sprintf("%d", v.Int)
	case 2:
		return fmt.Sprintf("%v", v.Float)
	}
	return ""
}

// AsBytes 以字节形式返回该值。
func (v sqlValue) AsBytes() []byte {
	switch v.Type {
	case 4:
		return v.Blob
	case 3:
		return []byte(v.Text)
	}
	return nil
}

const sqliteHeaderSize = 100

// openSQLite 解析 SQLite 文件头。
func openSQLite(data []byte) (*sqliteDB, error) {
	if len(data) < sqliteHeaderSize {
		return nil, errors.New("sqlite: file too small")
	}
	if string(data[:16]) != "SQLite format 3\x00" {
		return nil, errors.New("sqlite: bad magic header")
	}
	ps := int(binary.BigEndian.Uint16(data[16:18]))
	if ps == 1 {
		ps = 65536
	}
	if ps < 512 || ps > 65536 || ps&(ps-1) != 0 {
		return nil, fmt.Errorf("sqlite: invalid page size %d", ps)
	}
	reserved := int(data[20])
	return &sqliteDB{data: data, pageSize: ps, usable: ps - reserved}, nil
}

// pageBase 返回 1-based 页号的文件偏移。
func (db *sqliteDB) pageBase(n int) (int, error) {
	off := (n - 1) * db.pageSize
	if n < 1 || off+db.pageSize > len(db.data) {
		return 0, fmt.Errorf("sqlite: page %d out of range", n)
	}
	return off, nil
}

// page 返回 1-based 页号的页数据。
func (db *sqliteDB) page(n int) ([]byte, error) {
	off, err := db.pageBase(n)
	if err != nil {
		return nil, err
	}
	return db.data[off : off+db.pageSize], nil
}

// varint 解析 SQLite 变长整数。
func varint(b []byte, off int) (uint64, int) {
	var v uint64
	for i := 0; i < 8; i++ {
		if off+i >= len(b) {
			return v, len(b)
		}
		c := b[off+i]
		v = (v << 7) | uint64(c&0x7f)
		if c < 0x80 {
			return v, off + i + 1
		}
	}
	if off+8 >= len(b) {
		return v, len(b)
	}
	v = (v << 8) | uint64(b[off+8])
	return v, off + 9
}

// tableRoot 在 sqlite_master 中查找指定表的根页号。
func (db *sqliteDB) tableRoot(name string) (int, error) {
	rows, err := db.scanTree(1)
	if err != nil {
		return 0, err
	}
	for _, r := range rows {
		if len(r) < 4 {
			continue
		}
		// sqlite_master 列：type, name, tbl_name, rootpage, sql
		if r[0].AsString() == "table" && r[1].AsString() == name {
			return int(r[3].Int), nil
		}
	}
	return 0, fmt.Errorf("sqlite: table %q not found", name)
}

// scanTree 遍历以 root 为根的 b-tree，返回全部行（仅表 b-tree）。
func (db *sqliteDB) scanTree(root int) ([][]sqlValue, error) {
	var out [][]sqlValue
	seen := map[int]bool{}
	var walk func(n int) error
	walk = func(n int) error {
		if seen[n] {
			return nil
		}
		seen[n] = true
		p, err := db.page(n)
		if err != nil {
			return err
		}
		hdr := 0
		if n == 1 {
			hdr = sqliteHeaderSize
		}
		if hdr+8 > len(p) {
			return errors.New("sqlite: truncated page header")
		}
		typ := p[hdr]
		nCells := int(binary.BigEndian.Uint16(p[hdr+3 : hdr+5]))
		cellPtrOff := hdr + 8
		if typ == 0x05 {
			cellPtrOff = hdr + 12
		}
		if cellPtrOff+2*nCells > len(p) {
			return errors.New("sqlite: bad cell pointer array")
		}
		ptrs := make([]int, nCells)
		for i := 0; i < nCells; i++ {
			ptrs[i] = int(binary.BigEndian.Uint16(p[cellPtrOff+2*i : cellPtrOff+2*i+2]))
		}
		switch typ {
		case 0x05: // 内部表页
			for _, off := range ptrs {
				if off+4 > len(p) {
					return errors.New("sqlite: bad interior cell")
				}
				if err := walk(int(binary.BigEndian.Uint32(p[off : off+4]))); err != nil {
					return err
				}
			}
			if hdr+12 <= len(p) {
				if right := int(binary.BigEndian.Uint32(p[hdr+8 : hdr+12])); right > 0 {
					if err := walk(right); err != nil {
						return err
					}
				}
			}
		case 0x0D: // 叶子表页
			for _, off := range ptrs {
				raw, _, err := db.readCellPayload(p, off)
				if err != nil {
					continue // 单行损坏不放弃整表
				}
				rec, err := decodeRecord(raw)
				if err != nil {
					continue
				}
				out = append(out, rec)
			}
		default:
			return fmt.Errorf("sqlite: unsupported page type 0x%02x", typ)
		}
		return nil
	}
	if err := walk(root); err != nil {
		return nil, err
	}
	return out, nil
}

// localPayloadSize 计算 payload 中本地存储的字节数。
//
// 严格按 SQLite 文件格式文档：
//
//	X = U - 35
//	M = ((U - 12) * 32 / 255) - 23
//	K = M + ((P - M) % (U - 4))
//	P <= X        → local = P
//	否则 K <= X   → local = K
//	否则          → local = M
//
// 注意 M 才是取模基数里的那个偏移（不是 X）—— 用 X 会让带溢出页的记录
// 定位错位，进而整行读不出来。
func (db *sqliteDB) localPayloadSize(payloadLen int) int {
	u := db.usable
	x := u - 35
	if payloadLen <= x {
		return payloadLen
	}
	m := ((u - 12) * 32 / 255) - 23
	if m < 0 {
		m = 0
	}
	k := m + (payloadLen-m)%(u-4)
	if k <= x {
		return k
	}
	return m
}

// readCellPayload 解析一个叶子表单元，返回拼装完成的记录 payload 字节
// 以及 payload 在页内的起始偏移。
func (db *sqliteDB) readCellPayload(p []byte, off int) ([]byte, int, error) {
	payloadLen, o := varint(p, off)
	if o >= len(p) {
		return nil, 0, errors.New("sqlite: bad payload len")
	}
	if _, o2 := varint(p, o); o2 >= len(p) { // rowid，本用途不需要
		return nil, 0, errors.New("sqlite: bad rowid")
	} else {
		o = o2
	}
	pl := int(payloadLen)
	local := db.localPayloadSize(pl)
	avail := len(p) - o
	if local > avail {
		local = avail
	}

	buf := make([]byte, 0, pl)
	buf = append(buf, p[o:o+local]...)
	if local < pl {
		// 溢出页起始页号紧跟在本地数据之后
		nextOff := o + local
		if nextOff+4 > len(p) {
			return nil, 0, errors.New("sqlite: missing overflow pointer")
		}
		next := int(binary.BigEndian.Uint32(p[nextOff : nextOff+4]))
		for next > 0 && len(buf) < pl {
			op, err := db.page(next)
			if err != nil {
				return nil, 0, err
			}
			next = int(binary.BigEndian.Uint32(op[0:4]))
			take := db.usable - 4
			if remain := pl - len(buf); remain < take {
				take = remain
			}
			if 4+take > len(op) {
				take = len(op) - 4
			}
			buf = append(buf, op[4:4+take]...)
		}
		if len(buf) < pl {
			return nil, 0, errors.New("sqlite: overflow truncated")
		}
	}
	return buf[:pl], o, nil
}

// decodeRecord 解码一条记录（serial type 序列 + 值区）。
func decodeRecord(rec []byte) ([]sqlValue, error) {
	if len(rec) == 0 {
		return nil, errors.New("sqlite: empty record")
	}
	hdrSize, off := varint(rec, 0)
	if int(hdrSize) > len(rec) {
		return nil, errors.New("sqlite: bad record header size")
	}
	var types []uint64
	for off < int(hdrSize) {
		t, n := varint(rec, off)
		if n <= off {
			break
		}
		types = append(types, t)
		off = n
	}
	vals := make([]sqlValue, 0, len(types))
	body := int(hdrSize)
	for _, t := range types {
		switch {
		case t == 0:
			vals = append(vals, sqlValue{Type: 0})
		case t >= 1 && t <= 6:
			n := []int{0, 1, 2, 3, 4, 6, 8}[t]
			if body+n > len(rec) {
				return nil, errors.New("sqlite: truncated int")
			}
			var v int64
			for i := 0; i < n; i++ {
				v = v<<8 | int64(rec[body+i])
			}
			shift := uint(64 - 8*n)
			v = v << shift >> shift
			vals = append(vals, sqlValue{Type: 1, Int: v})
			body += n
		case t == 7:
			if body+8 > len(rec) {
				return nil, errors.New("sqlite: truncated float")
			}
			vals = append(vals, sqlValue{Type: 2, Float: float64FromBits(binary.BigEndian.Uint64(rec[body : body+8]))})
			body += 8
		case t == 8:
			vals = append(vals, sqlValue{Type: 1, Int: 0})
		case t == 9:
			vals = append(vals, sqlValue{Type: 1, Int: 1})
		case t >= 12:
			isBlob := t%2 == 0
			var n int
			if isBlob {
				n = int((t - 12) / 2)
			} else {
				n = int((t - 13) / 2)
			}
			if body+n > len(rec) {
				return nil, errors.New("sqlite: truncated value")
			}
			raw := rec[body : body+n]
			if isBlob {
				vals = append(vals, sqlValue{Type: 4, Blob: raw})
			} else {
				vals = append(vals, sqlValue{Type: 3, Text: string(raw)})
			}
			body += n
		default:
			return nil, fmt.Errorf("sqlite: unsupported serial type %d", t)
		}
	}
	return vals, nil
}

// --- 定位与原地覆写 ---

// cellLoc 描述一个叶子表单元在文件中的物理位置。
type cellLoc struct {
	pageNo   int    // 叶子页页号
	dataOff  int    // 记录 payload 在页内的偏移
	local    int    // 本地存储字节数
	payload  int    // 记录 payload 总长
	overflow []int  // 溢出页页号（按链序）
	raw      []byte // 拼装完成的记录 payload（用于计算列偏移）
}

// ReadItemValue 读取 table 中第一列等于 key 的那一行的第二列值。
func (db *sqliteDB) ReadItemValue(table, key string) ([]byte, error) {
	root, err := db.tableRoot(table)
	if err != nil {
		return nil, err
	}
	loc, err := db.locateCell(root, func(rec []sqlValue) bool {
		return len(rec) >= 2 && rec[0].AsString() == key
	})
	if err != nil {
		return nil, err
	}
	rec, err := decodeRecord(loc.raw)
	if err != nil {
		return nil, err
	}
	return rec[1].AsBytes(), nil
}

// PatchItemValue 把 table 中第一列等于 key 的那一行的第二列值原地覆写为 newValue。
//
// 只允许同长度覆写：不新增 / 删除 cell、不改页结构、不动 freelist，
// 因此不存在把数据库改坏的结构性风险。长度不一致时返回 ErrLengthMismatch。
func (db *sqliteDB) PatchItemValue(table, key string, newValue []byte) error {
	root, err := db.tableRoot(table)
	if err != nil {
		return err
	}
	loc, err := db.locateCell(root, func(rec []sqlValue) bool {
		return len(rec) >= 2 && rec[0].AsString() == key
	})
	if err != nil {
		return err
	}
	rec, err := decodeRecord(loc.raw)
	if err != nil {
		return err
	}
	old := rec[1].AsBytes()
	if len(newValue) != len(old) {
		return ErrLengthMismatch
	}
	off, err := valueOffset(loc.raw, 1)
	if err != nil {
		return err
	}
	return db.writeAt(loc, off, newValue)
}

// ErrLengthMismatch 表示新旧值长度不一致，无法原地覆写。
var ErrLengthMismatch = errors.New("sqlite: 新旧值长度不一致，无法原地覆写")

// locateCell 在以 root 为根的 b-tree 中找到第一个满足 match 的单元。
func (db *sqliteDB) locateCell(root int, match func([]sqlValue) bool) (*cellLoc, error) {
	seen := map[int]bool{}
	var found *cellLoc
	var walk func(n int) error
	walk = func(n int) error {
		if seen[n] || found != nil {
			return nil
		}
		seen[n] = true
		p, err := db.page(n)
		if err != nil {
			return err
		}
		hdr := 0
		if n == 1 {
			hdr = sqliteHeaderSize
		}
		if hdr+8 > len(p) {
			return errors.New("sqlite: truncated page header")
		}
		typ := p[hdr]
		nCells := int(binary.BigEndian.Uint16(p[hdr+3 : hdr+5]))
		cellPtrOff := hdr + 8
		if typ == 0x05 {
			cellPtrOff = hdr + 12
		}
		if cellPtrOff+2*nCells > len(p) {
			return errors.New("sqlite: bad cell pointer array")
		}
		ptrs := make([]int, nCells)
		for i := 0; i < nCells; i++ {
			ptrs[i] = int(binary.BigEndian.Uint16(p[cellPtrOff+2*i : cellPtrOff+2*i+2]))
		}
		switch typ {
		case 0x05:
			for _, off := range ptrs {
				if off+4 > len(p) {
					return errors.New("sqlite: bad interior cell")
				}
				if err := walk(int(binary.BigEndian.Uint32(p[off : off+4]))); err != nil {
					return err
				}
			}
			if hdr+12 <= len(p) {
				if right := int(binary.BigEndian.Uint32(p[hdr+8 : hdr+12])); right > 0 {
					if err := walk(right); err != nil {
						return err
					}
				}
			}
		case 0x0D:
			for _, off := range ptrs {
				raw, _, err := db.readCellPayload(p, off)
				if err != nil {
					continue
				}
				rec, err := decodeRecord(raw)
				if err != nil || !match(rec) {
					continue
				}
				loc, err := db.buildLoc(n, p, off)
				if err != nil {
					return err
				}
				loc.raw = raw
				found = loc
				return nil
			}
		}
		return nil
	}
	if err := walk(root); err != nil {
		return nil, err
	}
	if found == nil {
		return nil, errors.New("sqlite: cell not found")
	}
	return found, nil
}

// buildLoc 计算单元的本地长度与溢出页链。
func (db *sqliteDB) buildLoc(pageNo int, p []byte, cellOff int) (*cellLoc, error) {
	payloadLen, o := varint(p, cellOff)
	_, o = varint(p, o) // rowid
	pl := int(payloadLen)
	local := db.localPayloadSize(pl)
	if avail := len(p) - o; local > avail {
		local = avail
	}
	loc := &cellLoc{pageNo: pageNo, dataOff: o, local: local, payload: pl}
	if local < pl {
		nextOff := o + local
		if nextOff+4 > len(p) {
			return nil, errors.New("sqlite: missing overflow pointer")
		}
		next := int(binary.BigEndian.Uint32(p[nextOff : nextOff+4]))
		for next > 0 {
			loc.overflow = append(loc.overflow, next)
			if len(loc.overflow) > 1<<20 {
				return nil, errors.New("sqlite: overflow chain too long")
			}
			op, err := db.page(next)
			if err != nil {
				return nil, err
			}
			next = int(binary.BigEndian.Uint32(op[0:4]))
		}
	}
	return loc, nil
}

// writeAt 在记录 payload 的相对偏移 rel 处原地覆写 len(b) 字节。
//
// 只允许同长度覆写：调用方必须保证 rel+len(b) <= payload。
func (db *sqliteDB) writeAt(loc *cellLoc, rel int, b []byte) error {
	if rel < 0 || rel+len(b) > loc.payload {
		return errors.New("sqlite: write out of payload range")
	}
	// 先本地段
	if rel < loc.local {
		n := loc.local - rel
		if n > len(b) {
			n = len(b)
		}
		base, err := db.pageBase(loc.pageNo)
		if err != nil {
			return err
		}
		start := base + loc.dataOff + rel
		if start+n > len(db.data) {
			return errors.New("sqlite: local write out of file")
		}
		copy(db.data[start:start+n], b[:n])
		rel += n
		b = b[n:]
	}
	if len(b) == 0 {
		return nil
	}
	// 再溢出段
	off := rel - loc.local
	for _, pg := range loc.overflow {
		base, err := db.pageBase(pg)
		if err != nil {
			return err
		}
		cap := db.usable - 4
		if off >= cap {
			off -= cap
			continue
		}
		n := cap - off
		if n > len(b) {
			n = len(b)
		}
		start := base + 4 + off
		if start+n > len(db.data) {
			return errors.New("sqlite: overflow write out of file")
		}
		copy(db.data[start:start+n], b[:n])
		b = b[n:]
		off = 0
		if len(b) == 0 {
			return nil
		}
	}
	return errors.New("sqlite: overflow chain too short")
}

// valueOffset 返回记录中第 idx 个值的字节偏移（相对 payload 起点）。
func valueOffset(rec []byte, idx int) (int, error) {
	hdrSize, off := varint(rec, 0)
	if int(hdrSize) > len(rec) {
		return 0, errors.New("sqlite: bad record header size")
	}
	types := make([]uint64, 0, idx+1)
	for off < int(hdrSize) {
		t, n := varint(rec, off)
		if n <= off {
			break
		}
		types = append(types, t)
		off = n
	}
	if idx >= len(types) {
		return 0, errors.New("sqlite: value index out of range")
	}
	body := int(hdrSize)
	for i := 0; i < idx; i++ {
		n, err := serialLen(types[i])
		if err != nil {
			return 0, err
		}
		body += n
	}
	return body, nil
}

// serialLen 返回某个 serial type 占用的字节数。
func serialLen(t uint64) (int, error) {
	switch {
	case t == 0, t == 8, t == 9:
		return 0, nil
	case t >= 1 && t <= 4:
		return int(t), nil
	case t == 5:
		return 6, nil
	case t == 6, t == 7:
		return 8, nil
	case t >= 12:
		if t%2 == 0 {
			return int((t - 12) / 2), nil
		}
		return int((t - 13) / 2), nil
	}
	return 0, fmt.Errorf("sqlite: unsupported serial type %d", t)
}
