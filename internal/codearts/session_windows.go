//go:build windows

package codearts

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"
)

// CodeArts Agent 是 VS Code fork，其 secret storage 用的是 Electron 的 OSCrypt：
//
//	Local State:  os_crypt.encrypted_key = base64("DPAPI" + DPAPI(32 字节 AES 主密钥))
//	state.vscdb:  ItemTable.value = {"type":"Buffer","data":["v10" | nonce(12) | AES-256-GCM(ct||tag)]}
//
// 与 Chromium 完全一致（豆包桌面端也是同一套），因此这里复用同一套解密逻辑。

var (
	crypt32                = syscall.NewLazyDLL("crypt32.dll")
	procCryptUnprotectData = crypt32.NewProc("CryptUnprotectData")
	procCryptProtectData   = crypt32.NewProc("CryptProtectData")
)

type dataBlob struct {
	cbData uint32
	pbData *byte
}

// dpapiUnprotect 调用 CryptUnprotectData 解密一段 DPAPI 保护的数据。
func dpapiUnprotect(in []byte) ([]byte, error) {
	if len(in) == 0 {
		return nil, errors.New("dpapi: empty input")
	}
	var inBlob, outBlob dataBlob
	inBlob.cbData = uint32(len(in))
	inBlob.pbData = &in[0]

	r, _, err := procCryptUnprotectData.Call(
		uintptr(unsafe.Pointer(&inBlob)),
		0, 0, 0, 0, 0,
		uintptr(unsafe.Pointer(&outBlob)),
	)
	if r == 0 {
		return nil, fmt.Errorf("dpapi: CryptUnprotectData failed: %w", err)
	}
	defer syscall.LocalFree(syscall.Handle(unsafe.Pointer(outBlob.pbData)))

	out := make([]byte, outBlob.cbData)
	copy(out, unsafe.Slice(outBlob.pbData, outBlob.cbData))
	return out, nil
}

// dpapiProtect 调用 CryptProtectData 加密数据（回写 Local State 时不需要，保留备用）。
func dpapiProtect(in []byte) ([]byte, error) {
	var inBlob, outBlob dataBlob
	inBlob.cbData = uint32(len(in))
	inBlob.pbData = &in[0]
	r, _, err := procCryptProtectData.Call(
		uintptr(unsafe.Pointer(&inBlob)),
		0, 0, 0, 0, 0,
		uintptr(unsafe.Pointer(&outBlob)),
	)
	if r == 0 {
		return nil, fmt.Errorf("dpapi: CryptProtectData failed: %w", err)
	}
	defer syscall.LocalFree(syscall.Handle(unsafe.Pointer(outBlob.pbData)))
	out := make([]byte, outBlob.cbData)
	copy(out, unsafe.Slice(outBlob.pbData, outBlob.cbData))
	return out, nil
}

// localStateKey 从 Local State 取出并解密 Chromium 的 AES 主密钥。
func localStateKey(userData string) ([]byte, error) {
	raw, err := os.ReadFile(filepath.Join(userData, "Local State"))
	if err != nil {
		return nil, err
	}
	var ls struct {
		OSCrypt struct {
			EncryptedKey string `json:"encrypted_key"`
		} `json:"os_crypt"`
	}
	if err := json.Unmarshal(raw, &ls); err != nil {
		return nil, fmt.Errorf("Local State 解析失败: %w", err)
	}
	if ls.OSCrypt.EncryptedKey == "" {
		return nil, errors.New("Local State 中缺少 os_crypt.encrypted_key")
	}
	blob, err := base64.StdEncoding.DecodeString(ls.OSCrypt.EncryptedKey)
	if err != nil {
		return nil, err
	}
	if len(blob) < 5 || string(blob[:5]) != "DPAPI" {
		return nil, errors.New("encrypted_key 前缀不是 DPAPI")
	}
	return dpapiUnprotect(blob[5:])
}

// aesGCMDecrypt 解密 "v10" 前缀的 OSCrypt 密文。
func aesGCMDecrypt(key, raw []byte) ([]byte, error) {
	if len(raw) < 3+12+16 {
		return nil, errors.New("密文过短")
	}
	if string(raw[:3]) != "v10" {
		return nil, errors.New("密文前缀不是 v10")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return gcm.Open(nil, raw[3:15], raw[15:], nil)
}

// aesGCMEncrypt 生成 "v10" 前缀的 OSCrypt 密文。
func aesGCMEncrypt(key, plain []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := randRead(nonce); err != nil {
		return nil, err
	}
	out := make([]byte, 0, 3+len(nonce)+len(plain)+gcm.Overhead())
	out = append(out, 'v', '1', '0')
	out = append(out, nonce...)
	return gcm.Seal(out, nonce, plain, nil), nil
}

// readSecretValue 从 state.vscdb 读出某个 secret 的明文 JSON。
func readSecretValue(userData, itemKey string) ([]byte, error) {
	dbPath := filepath.Join(userData, "User", "globalStorage", "state.vscdb")
	data, err := readFileAll(dbPath)
	if err != nil {
		return nil, err
	}
	db, err := openSQLite(data)
	if err != nil {
		return nil, fmt.Errorf("state.vscdb 解析失败: %w", err)
	}
	root, err := db.tableRoot("ItemTable")
	if err != nil {
		return nil, err
	}
	rows, err := db.scanTree(root)
	if err != nil {
		return nil, err
	}
	var encoded []byte
	for _, r := range rows {
		if len(r) >= 2 && r[0].AsString() == itemKey {
			encoded = r[1].AsBytes()
			break
		}
	}
	if encoded == nil {
		return nil, errors.New("state.vscdb 中未找到 CodeArts 登录会话（请先在 IDE 里登录）")
	}
	key, err := localStateKey(userData)
	if err != nil {
		return nil, err
	}
	return decryptSecretPayload(key, encoded)
}

// decryptSecretPayload 把 ItemTable 里的 {"type":"Buffer","data":[…]} 解成明文 JSON。
func decryptSecretPayload(masterKey, encoded []byte) ([]byte, error) {
	var wrap struct {
		Type string `json:"type"`
		Data []byte `json:"data"`
	}
	if err := json.Unmarshal(encoded, &wrap); err != nil {
		return nil, fmt.Errorf("secret 包装解析失败: %w", err)
	}
	if len(wrap.Data) == 0 {
		return nil, errors.New("secret 内容为空")
	}
	return aesGCMDecrypt(masterKey, wrap.Data)
}

// ReadSession 读取并解密 CodeArts Agent 的登录会话。
func ReadSession(userData string) (*Session, error) {
	plain, err := readSecretValue(userData, SecretItemKey)
	if err != nil {
		return nil, err
	}
	return parseSession(plain)
}

// writeBackSession 尽力把续期后的会话回写 state.vscdb。
//
// 只在「新密文长度不超过旧密文」时执行原地字节覆写（不足处用尾随空格补齐，
// JSON 允许尾随空白），因此：
//   - 不新增 / 删除 cell，不改页结构，不动 freelist；
//   - 不会触发页分裂或数据库重整；
//   - 写入前一定先备份一份 .codearts2api.bak。
//
// 长度超出（刷新令牌变长等）时返回 ErrWriteBackSkipped，
// 由调用方降级为「仅在网关内保存」。
func writeBackSession(userData string, sess *Session) error {
	dbPath := filepath.Join(userData, "User", "globalStorage", "state.vscdb")
	data, err := readFileAll(dbPath)
	if err != nil {
		return err
	}
	db, err := openSQLite(data)
	if err != nil {
		return err
	}

	plain, err := sess.RawJSON()
	if err != nil {
		return err
	}
	masterKey, err := localStateKey(userData)
	if err != nil {
		return err
	}
	oldValue, err := db.ReadItemValue("ItemTable", SecretItemKey)
	if err != nil {
		return err
	}

	blob, err := aesGCMEncrypt(masterKey, plain)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(map[string]any{"type": "Buffer", "data": blob})
	if err != nil {
		return err
	}
	if len(encoded) > len(oldValue) {
		return ErrWriteBackSkipped
	}
	// JSON 允许尾随空白：直接在包装 JSON 后面补空格对齐长度。
	//
	// 不能靠「给明文补空格」来对齐 —— data 是密文字节的十进制数组，
	// 每个字节占 2~4 个字符，长度不可预测。补在 JSON 外层是确定性的。
	for len(encoded) < len(oldValue) {
		encoded = append(encoded, ' ')
	}

	// 备份一次，便于人工回滚
	if err := copyFile(dbPath, dbPath+".codearts2api.bak"); err != nil {
		return fmt.Errorf("备份 state.vscdb 失败: %w", err)
	}
	if err := db.PatchItemValue("ItemTable", SecretItemKey, encoded); err != nil {
		return err
	}
	return writeFileAtomic(dbPath, db.data)
}

// ErrWriteBackSkipped 表示回写因长度不一致被安全跳过。
var ErrWriteBackSkipped = errors.New("回写 state.vscdb 被跳过（长度不一致，无法原地覆写）")

// copyFile 复制文件（用于写前备份）。
func copyFile(src, dst string) error {
	in, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, in, 0o600)
}

// writeFileAtomic 原子写回文件。
func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".codearts2api.tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// DefaultUserDataDirs 返回本机 CodeArts Agent 的 User Data 目录候选。
func DefaultUserDataDirs(explicit string) []string {
	if explicit != "" {
		return []string{explicit}
	}
	var out []string
	for _, base := range []string{os.Getenv("APPDATA"), os.Getenv("LOCALAPPDATA")} {
		if base == "" {
			continue
		}
		out = append(out, filepath.Join(base, "codearts-agent"))
	}
	return out
}
