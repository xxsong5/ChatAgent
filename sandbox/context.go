package sandbox

import "context"

// sandboxKey 用于在context中存储container instance的key
type sandboxKey struct{}

// WithContainer 在context中注入Container
// 这样tool层可以通过context获取到sandbox instance来执行命令
func WithContainer(ctx context.Context, instance Container) context.Context {
	return context.WithValue(ctx, sandboxKey{}, instance)
}

// GetContainer 从context中获取Container
func GetContainer(ctx context.Context) Container {
	instance, ok := ctx.Value(sandboxKey{}).(Container)
	if !ok {
		return nil
	}
	return instance
}
