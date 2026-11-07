//go:build darwin

package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"

	"codexrouter/internal/platform"
)

func init() { platformCommand = runKeychainCommand }

// Read only the fixed provider IDs in state.json; no Store.Open, lock, writes or key creation.
func keychainNames(data string) ([]string, error) {
	names := []string{"control"}
	file := filepath.Join(data, "state.json")
	info, err := os.Lstat(file)
	if errors.Is(err, os.ErrNotExist) {
		return names, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > 16<<20 {
		return nil, errors.New("无法安全读取供应商凭据名称；未修改任何凭据")
	}
	f, err := os.Open(file)
	if err != nil {
		return nil, errors.New("供应商状态不可读；未修改任何凭据")
	}
	defer f.Close()
	var state struct {
		Providers []struct {
			ID   string `json:"id"`
			Kind string `json:"kind"`
		} `json:"providers"`
	}
	raw, err := io.ReadAll(io.LimitReader(f, (16<<20)+1))
	if err != nil || len(raw) > 16<<20 || json.Unmarshal(raw, &state) != nil || len(state.Providers) > 8 {
		return nil, errors.New("供应商状态不可识别；未修改任何凭据")
	}
	seen := map[string]bool{"control": true}
	valid := regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)
	for _, provider := range state.Providers {
		if provider.Kind != "api_key" {
			continue
		}
		if !valid.MatchString(provider.ID) || seen[provider.ID] {
			return nil, errors.New("供应商凭据名称无效；未修改任何凭据")
		}
		seen[provider.ID] = true
		names = append(names, provider.ID)
	}
	return names, nil
}

func runKeychainCommand(args []string) (bool, error) {
	if len(args) > 0 && args[0] == "keychain-profile" {
		return true, runKeychainProfileCommand(args[1:])
	}
	if len(args) == 0 || (args[0] != "keychain-check" && args[0] != "keychain-authorize") {
		return false, nil
	}
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	data := flags.String("data-dir", "", "检查已有数据目录，不创建目录或凭据")
	if err := flags.Parse(args[1:]); err != nil {
		return true, err
	}
	if flags.NArg() != 0 {
		return true, errors.New("仅接受 --data-dir")
	}
	if *data == "" {
		dir, err := platform.DefaultDataDir()
		if err != nil {
			return true, err
		}
		*data = dir
	}
	identity, err := platform.DataDirectoryIdentity(*data)
	if err != nil {
		return true, err
	}
	info, err := os.Stat(identity)
	if err != nil || !info.IsDir() {
		return true, errors.New("数据目录不存在；此命令不会创建目录或凭据")
	}
	names, err := keychainNames(identity)
	if err != nil {
		return true, err
	}
	credentials, err := platform.OpenCredentials(identity)
	if err != nil {
		return true, err
	}
	access, ok := credentials.(platform.CredentialAccess)
	if !ok {
		return true, platform.ErrUnsupported
	}
	authorize := args[0] == "keychain-authorize"
	if authorize {
		fmt.Println("仅为此构建请求已有 CodexRouter 条目的系统访问授权；请自行核对 macOS 弹窗。拒绝或取消将失败；不会删除/重建条目、修改全局 ACL 或输出凭据。")
	}
	failed := false
	for _, name := range names {
		err := access.Check(name)
		if authorize && err != nil && !errors.Is(err, platform.ErrNotFound) {
			err = access.Authorize(name)
		}
		if err == nil {
			fmt.Printf("%s: access=ok\n", name)
			continue
		}
		if errors.Is(err, platform.ErrNotFound) {
			fmt.Printf("%s: access=not_found (未创建)\n", name)
			if name == "control" {
				failed = true
			}
			continue
		}
		var status *platform.CredentialError
		if errors.As(err, &status) {
			fmt.Printf("%s: access=denied OSStatus=%d\n", name, status.Status)
		} else {
			fmt.Printf("%s: access=failed\n", name)
		}
		failed = true
	}
	if failed {
		return true, errors.New("本构建尚未取得已有 Keychain 条目访问权；保留运行中的旧版，请用新构建的 keychain-authorize 命令由本人完成系统授权后再更新")
	}
	if authorize {
		fmt.Println("访问检查通过。若选择了单次允许，重启前仍需运行 keychain-check；长期跨版本授权需要稳定签名及系统认可。")
	}
	return true, nil
}
