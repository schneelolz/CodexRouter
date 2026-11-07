//go:build darwin

package main

import (
	"errors"
	"flag"
	"fmt"

	"codexrouter/internal/platform"
)

func runKeychainProfileCommand(args []string) error {
	if len(args) == 0 {
		return errors.New("用法: keychain-profile preview|prepare|check|activate|rollback [--data-dir 路径] [--confirm]")
	}
	action := args[0]
	switch action {
	case "preview", "prepare", "check", "activate", "rollback":
	default:
		return errors.New("不支持的凭据空间操作")
	}
	flags := flag.NewFlagSet("keychain-profile "+action, flag.ContinueOnError)
	data := flags.String("data-dir", "", "已有数据目录")
	confirm := flags.Bool("confirm", false, "显式创建/选择新的空凭据空间；旧条目保留，供应商密钥需重新录入")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("凭据空间参数无效")
	}
	if *data == "" {
		dir, err := platform.DefaultDataDir()
		if err != nil {
			return err
		}
		*data = dir
	}
	names, err := keychainNames(*data)
	if err != nil {
		return err
	}
	state, err := platform.CredentialProfiles(*data)
	if err != nil {
		return err
	}
	if action == "preview" {
		fmt.Printf("active_new_profile=%t pending_new_profile=%t api_key_providers_requiring_reentry=%d\n", state.Active, state.Pending, len(names)-1)
		fmt.Println("仅新增凭据空间选择文件与新管理凭据；原钥匙串条目、供应商配置、会话绑定、历史和日常接入恢复记录保留。供应商API Key需在网页重新录入；不会自动换线。")
		return nil
	}
	if action == "check" {
		credentials, err := platform.OpenPreparedCredentials(*data)
		if err != nil {
			return err
		}
		access, ok := credentials.(platform.CredentialAccess)
		if !ok {
			return platform.ErrUnsupported
		}
		for _, name := range names {
			err := access.Check(name)
			if err == nil {
				fmt.Printf("%s: prepared_access=ok\n", name)
				continue
			}
			if name != "control" && errors.Is(err, platform.ErrNotFound) {
				fmt.Printf("%s: prepared_access=not_found (需重新录入)\n", name)
				continue
			}
			return fmt.Errorf("新空间访问检查失败: %w", err)
		}
		return nil
	}
	if !*confirm {
		return errors.New("此操作须显式 --confirm；先使用 preview 查看影响")
	}
	switch action {
	case "prepare":
		if err := platform.PrepareCredentialProfile(*data); err != nil {
			return err
		}
		fmt.Println("新空间已准备，当前空间未切换；请先运行 check，激活需要目标路由器已正常退出。")
	case "activate":
		if err := platform.ActivateCredentialProfile(*data); err != nil {
			return err
		}
		fmt.Println("新空间已激活，供应商密钥需在网页重新录入；旧条目保留，原会话供应商绑定未改变。")
	case "rollback":
		if err := platform.RollbackCredentialProfile(*data); err != nil {
			return err
		}
		fmt.Println("已恢复选择原凭据空间，新旧条目均保留。运行旧构建前仍须正常退出当前路由器。")
	}
	return nil
}
