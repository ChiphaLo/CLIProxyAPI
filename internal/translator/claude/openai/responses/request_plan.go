package responses

import (
	"strings"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/translator/common"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
)

// responsesRequestPlan materializes the input array once. Subsequent ordered
// passes walk borrowed Results rather than rescanning the original JSON array.
type responsesRequestPlan struct {
	Root                 ojson.Result
	Get                  func(string) ojson.Result
	RawItems, InputItems []ojson.Result
	Descriptors          []responsesToolDescriptor
	HistoryIdentities    []string
	ToolResults          map[string]ojson.Result
	ResizeNotices        map[int]string
	WebSearch            responsesWebSearchHistory
}

func newResponsesRequestPlan(root ojson.Result, model string, search bool) responsesRequestPlan {
	p := responsesRequestPlan{Root: root, Get: util.IndexRootFields(root), ToolResults: map[string]ojson.Result{}}
	input := p.Get("input")
	if input.IsArray() {
		p.RawItems = input.Array()
	}
	p.ResizeNotices = responsesInlineImageResizeNoticesFromItems(model, p.RawItems)
	sources := []responsesToolSource{}
	if tools := p.Get("tools"); tools.IsArray() {
		sources = append(sources, responsesToolSource{tools: tools, priority: 0})
	}
	var discovered []responsesToolSource
	for _, item := range p.RawItems {
		switch item.Get("type").String() {
		case "additional_tools":
			if tools := item.Get("tools"); tools.IsArray() {
				sources = append(sources, responsesToolSource{tools: tools, priority: 1})
			}
		case "tool_search_output":
			if tools := item.Get("tools"); search && tools.IsArray() {
				discovered = append(discovered, responsesToolSource{tools: tools, priority: responsesDiscoveredToolPriority})
			}
		case "function_call", "custom_tool_call":
			name := item.Get("name").String()
			if ns := strings.TrimSpace(item.Get("namespace").String()); ns != "" {
				name = qualifyResponsesNamespaceToolName(ns, name)
			}
			if name != "" {
				p.HistoryIdentities = append(p.HistoryIdentities, name)
			}
		}
	}
	sources = append(sources, discovered...)
	p.Descriptors = responsesToolDescriptorsFromSources(sources, search)
	p.InputItems = common.NormalizeResponsesToolCallOutputs(p.RawItems)
	for _, item := range p.InputItems {
		switch item.Get("type").String() {
		case "function_call_output", "custom_tool_call_output":
			if id := common.ExtractResponsesCallID(item); id != "" {
				p.ToolResults[id] = item
			}
		}
	}
	p.WebSearch = scanResponsesWebSearchHistory(p.InputItems)
	return p
}
