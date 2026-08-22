package repository

import (
	"context"

	dbent "github.com/Wei-Shaw/sub2api/ent"
)

// scanOneString 读一行单列字符串。
//
// ent 的 *Client 只暴露 QueryContext/ExecContext（没有 QueryRowContext），
// 而在事务里必须用同一个 exec client 查询，所以只能手动扫一行。
// 仓库里 group_repo.DeleteCascade 早先就是这么写的，这里把它收敛成一个 helper。
//
// found=false 表示没有匹配行 —— 这是正常结果，不是错误。
func scanOneString(ctx context.Context, exec *dbent.Client, query string, args ...any) (string, bool, error) {
	rows, err := exec.QueryContext(ctx, query, args...)
	if err != nil {
		return "", false, err
	}
	var value string
	found := rows.Next()
	if found {
		if scanErr := rows.Scan(&value); scanErr != nil {
			_ = rows.Close()
			return "", false, scanErr
		}
	}
	if closeErr := rows.Close(); closeErr != nil {
		return "", false, closeErr
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return "", false, rowsErr
	}
	return value, found, nil
}
