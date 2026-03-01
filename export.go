package toolssessions

import (
	"context"

	pluginv1 "github.com/orchestra-mcp/gen-go/orchestra/plugin/v1"
	"github.com/orchestra-mcp/plugin-tools-sessions/internal"
	"github.com/orchestra-mcp/plugin-tools-sessions/internal/storage"
	"github.com/orchestra-mcp/sdk-go/plugin"
)

// Sender is the interface that the in-process router satisfies.
type Sender interface {
	Send(ctx context.Context, req *pluginv1.PluginRequest) (*pluginv1.PluginResponse, error)
}

// Register adds all 6 session management tools to the builder.
func Register(builder *plugin.PluginBuilder, sender Sender) {
	store := storage.NewDataStorage(sender)
	tp := &internal.ToolsPlugin{Storage: store}
	tp.RegisterTools(builder)
}
