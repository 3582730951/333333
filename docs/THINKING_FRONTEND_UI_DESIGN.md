# Thinking 功能前端 UI 详细设计

## 一、UI 组件概览

### 1.1 导航栏新增项

在 `index.html` 的侧边栏"配置"区块添加新菜单项：

```html
<div class="nav-section">
    <div class="nav-section-title">配置</div>
    <!-- 现有项... -->
    <div class="nav-item" data-section="thinking">
        <span class="nav-icon">🧠</span>
        <span>Thinking 配置</span>
    </div>
</div>
```

---

## 二、主配置面板设计

### 2.1 整体布局

```
┌────────────────────────────────────────────────────────────┐
│  🧠 Thinking 深度思考配置                      [保存配置]  │
├────────────────────────────────────────────────────────────┤
│                                                              │
│  ┌─ 全局配置 ─────────────────────────────────────┐       │
│  │ ☑ 启用 Thinking 注入                            │       │
│  │                                                  │       │
│  │ 默认模式:  [级别模式 ▾]                        │       │
│  │ 默认级别:  ●────●────●────●────●  Medium       │       │
│  │           minimal low  med  high xhigh          │       │
│  │                                                  │       │
│  │ ℹ️ 思考强度越高，AI 回复质量越好，但消耗更多 token │       │
│  └──────────────────────────────────────────────────┘       │
│                                                              │
│  ┌─ 提供商覆盖 ────────────────────────────────────┐       │
│  │ Claude:  [级别模式 ▾]  Level: [High ▾]  💾      │       │
│  │ Codex:   [级别模式 ▾]  Level: [Medium ▾]  💾    │       │
│  │                                            [+新增]│       │
│  └──────────────────────────────────────────────────┘       │
│                                                              │
│  ┌─ 模型级覆盖 ────────────────────────────────────┐       │
│  │ ┌───────────────────────────────────────────────┐│       │
│  │ │模型名              │模式  │级别/预算│操作     ││       │
│  │ ├───────────────────────────────────────────────┤│       │
│  │ │claude-opus-4-8    │级别  │Max      │🗑️ 💾   ││       │
│  │ │gpt-5.2            │级别  │High     │🗑️ 💾   ││       │
│  │ └───────────────────────────────────────────────┘│       │
│  │                                            [+新增]│       │
│  └──────────────────────────────────────────────────┘       │
│                                                              │
│  ┌─ 实时预览 ──────────────────────────────────────┐       │
│  │ Provider: [Claude ▾]  Model: [claude-opus-4-8 ▾] │       │
│  │                                          [预览配置]│       │
│  │                                                  │       │
│  │ 当前请求会应用的配置:                             │       │
│  │ • 解析来源: 模型级覆盖                            │       │
│  │ • 应用配置: Mode=Level, Level=Max               │       │
│  │ • 上游 JSON:                                     │       │
│  │   {                                              │       │
│  │     "thinking": {"type": "adaptive"},            │       │
│  │     "output_config": {"effort": "max"}           │       │
│  │   }                                              │       │
│  └──────────────────────────────────────────────────┘       │
└────────────────────────────────────────────────────────────┘
```

---

## 三、HTML 结构

### 3.1 完整 HTML 代码

```html
<!-- 在 index.html 的 <main> 区域添加新 section -->
<div id="thinking" class="section">
    <div class="card">
        <div class="card-header">
            <h3 class="card-title">🧠 Thinking 深度思考配置</h3>
            <button class="btn btn-primary" onclick="saveThinkingConfig()">💾 保存配置</button>
        </div>

        <!-- 全局配置 -->
        <div class="config-section">
            <h4 class="section-title">全局配置</h4>
            <div class="form-group">
                <label class="toggle-label">
                    <input type="checkbox" id="thinkingEnabled" onchange="toggleThinkingEnabled()">
                    <span>启用 Thinking 注入</span>
                </label>
            </div>

            <div class="form-group">
                <label>默认模式</label>
                <select id="defaultMode" onchange="updateDefaultModeUI()">
                    <option value="level">级别模式 (Level)</option>
                    <option value="budget">预算模式 (Budget)</option>
                    <option value="auto">自动模式 (Auto)</option>
                    <option value="none">禁用 (None)</option>
                </select>
            </div>

            <div id="defaultLevelGroup" class="form-group">
                <label>默认级别 <span id="levelValue">medium</span></label>
                <div class="slider-container">
                    <input type="range" id="defaultLevel" min="0" max="5" value="2" 
                           oninput="updateLevelDisplay()" class="slider">
                    <div class="slider-labels">
                        <span>minimal</span>
                        <span>low</span>
                        <span>medium</span>
                        <span>high</span>
                        <span>xhigh</span>
                        <span>max</span>
                    </div>
                </div>
            </div>

            <div id="defaultBudgetGroup" class="form-group" style="display: none;">
                <label>默认预算 (Tokens)</label>
                <input type="number" id="defaultBudget" value="8192" 
                       min="512" max="128000" step="512" class="form-control">
            </div>

            <div class="info-box">
                <span class="info-icon">ℹ️</span>
                思考强度越高，AI 回复质量越好，但会消耗更多 token
            </div>
        </div>

        <!-- 提供商覆盖 -->
        <div class="config-section">
            <h4 class="section-title">提供商覆盖</h4>
            <div id="providerOverrides">
                <!-- 动态生成 -->
            </div>
            <button class="btn btn-secondary" onclick="addProviderOverride()">+ 新增提供商</button>
        </div>

        <!-- 模型级覆盖 -->
        <div class="config-section">
            <h4 class="section-title">模型级覆盖</h4>
            <div class="table-container">
                <table id="modelOverridesTable">
                    <thead>
                        <tr>
                            <th>模型名</th>
                            <th>模式</th>
                            <th>级别/预算</th>
                            <th>操作</th>
                        </tr>
                    </thead>
                    <tbody id="modelOverridesBody">
                        <!-- 动态生成 -->
                    </tbody>
                </table>
            </div>
            <button class="btn btn-secondary" onclick="addModelOverride()">+ 新增模型</button>
        </div>

        <!-- 实时预览 -->
        <div class="config-section">
            <h4 class="section-title">实时预览</h4>
            <div class="preview-controls">
                <select id="previewProvider" onchange="updatePreview()">
                    <option value="claude">Claude</option>
                    <option value="codex">Codex</option>
                </select>
                <input type="text" id="previewModel" placeholder="模型名 (如 claude-opus-4-8)" 
                       class="form-control" onchange="updatePreview()">
                <button class="btn btn-secondary" onclick="updatePreview()">🔄 预览配置</button>
            </div>
            <div id="previewResult" class="preview-box">
                <!-- 预览结果 -->
            </div>
        </div>
    </div>
</div>
```

### 3.2 CSS 样式（添加到 app.css 或内联）

```css
/* Thinking 配置页面样式 */
.config-section {
    margin-bottom: 32px;
    padding-bottom: 32px;
    border-bottom: 1px solid var(--border);
}

.config-section:last-child {
    border-bottom: none;
}

.section-title {
    font-size: 16px;
    font-weight: 600;
    margin-bottom: 16px;
    color: var(--text);
}

.form-group {
    margin-bottom: 20px;
}

.form-group label {
    display: block;
    font-size: 14px;
    font-weight: 500;
    margin-bottom: 8px;
    color: var(--text);
}

.toggle-label {
    display: flex;
    align-items: center;
    gap: 12px;
    cursor: pointer;
}

.toggle-label input[type="checkbox"] {
    width: 20px;
    height: 20px;
    cursor: pointer;
}

.form-control, select {
    width: 100%;
    padding: 10px 12px;
    border: 1px solid var(--border);
    border-radius: 6px;
    font-size: 14px;
    background: var(--surface);
    color: var(--text);
}

.form-control:focus, select:focus {
    outline: none;
    border-color: var(--primary);
    box-shadow: 0 0 0 3px rgba(59, 130, 246, 0.1);
}

/* 滑块样式 */
.slider-container {
    position: relative;
    padding-bottom: 24px;
}

.slider {
    width: 100%;
    height: 6px;
    border-radius: 3px;
    background: linear-gradient(to right, 
        #10b981 0%, #10b981 20%, 
        #3b82f6 20%, #3b82f6 40%,
        #f59e0b 40%, #f59e0b 60%,
        #ef4444 60%, #ef4444 80%,
        #8b5cf6 80%, #8b5cf6 100%);
    outline: none;
    appearance: none;
    cursor: pointer;
}

.slider::-webkit-slider-thumb {
    appearance: none;
    width: 20px;
    height: 20px;
    border-radius: 50%;
    background: var(--surface);
    border: 3px solid var(--primary);
    cursor: pointer;
    box-shadow: 0 2px 4px rgba(0,0,0,0.1);
}

.slider::-moz-range-thumb {
    width: 20px;
    height: 20px;
    border-radius: 50%;
    background: var(--surface);
    border: 3px solid var(--primary);
    cursor: pointer;
    box-shadow: 0 2px 4px rgba(0,0,0,0.1);
}

.slider-labels {
    display: flex;
    justify-content: space-between;
    margin-top: 8px;
    padding: 0 10px;
}

.slider-labels span {
    font-size: 11px;
    color: var(--text-secondary);
}

/* 信息框 */
.info-box {
    display: flex;
    align-items: center;
    gap: 12px;
    padding: 12px 16px;
    background: #eff6ff;
    border: 1px solid #bfdbfe;
    border-radius: 8px;
    font-size: 14px;
    color: #1e40af;
}

.info-icon {
    font-size: 18px;
}

/* 提供商覆盖卡片 */
.provider-override-card {
    display: grid;
    grid-template-columns: 120px 1fr 1fr auto;
    gap: 16px;
    align-items: center;
    padding: 16px;
    background: var(--bg);
    border-radius: 8px;
    margin-bottom: 12px;
}

.provider-override-card label {
    font-weight: 500;
    margin: 0;
}

/* 预览框 */
.preview-controls {
    display: grid;
    grid-template-columns: 150px 1fr auto;
    gap: 12px;
    margin-bottom: 16px;
}

.preview-box {
    padding: 16px;
    background: #1e293b;
    color: #e2e8f0;
    border-radius: 8px;
    font-family: 'Courier New', monospace;
    font-size: 13px;
    line-height: 1.6;
    max-height: 400px;
    overflow-y: auto;
}

.preview-box pre {
    margin: 0;
    white-space: pre-wrap;
    word-wrap: break-word;
}

.preview-label {
    color: #94a3b8;
    font-weight: 600;
}

.preview-value {
    color: #10b981;
}

/* 表格操作按钮 */
.action-btn {
    padding: 6px 12px;
    border: none;
    border-radius: 4px;
    cursor: pointer;
    font-size: 16px;
    background: none;
    transition: background 0.2s;
}

.action-btn:hover {
    background: var(--bg);
}

.action-btn.delete:hover {
    background: #fee;
    color: var(--danger);
}
```

---

## 四、JavaScript 交互逻辑

### 4.1 核心 JavaScript 代码

```javascript
// ============ Thinking 配置管理 ============

// 全局状态
let thinkingConfig = {
    enabled: false,
    default_mode: 'level',
    default_level: 'medium',
    default_budget: 8192,
    providers: {},
    models: {}
};

// 级别映射
const LEVEL_MAP = ['minimal', 'low', 'medium', 'high', 'xhigh', 'max'];
const LEVEL_BUDGET_MAP = {
    'minimal': 512,
    'low': 1024,
    'medium': 8192,
    'high': 24576,
    'xhigh': 32768,
    'max': 128000
};

// 加载配置
async function loadThinkingConfig() {
    try {
        const resp = await fetch('/admin/thinking', {
            headers: { 'Authorization': `Bearer ${getToken()}` }
        });
        if (!resp.ok) {
            throw new Error(`HTTP ${resp.status}`);
        }
        thinkingConfig = await resp.json();
        renderThinkingUI();
    } catch (err) {
        console.error('Failed to load thinking config:', err);
        showNotification('加载 Thinking 配置失败: ' + err.message, 'error');
    }
}

// 渲染 UI
function renderThinkingUI() {
    // 全局开关
    document.getElementById('thinkingEnabled').checked = thinkingConfig.enabled;
    
    // 默认模式
    document.getElementById('defaultMode').value = thinkingConfig.default_mode || 'level';
    updateDefaultModeUI();
    
    // 默认级别
    const levelIndex = LEVEL_MAP.indexOf(thinkingConfig.default_level || 'medium');
    document.getElementById('defaultLevel').value = levelIndex >= 0 ? levelIndex : 2;
    updateLevelDisplay();
    
    // 默认预算
    document.getElementById('defaultBudget').value = thinkingConfig.default_budget || 8192;
    
    // 提供商覆盖
    renderProviderOverrides();
    
    // 模型覆盖
    renderModelOverrides();
}

// 更新默认模式 UI
function updateDefaultModeUI() {
    const mode = document.getElementById('defaultMode').value;
    const levelGroup = document.getElementById('defaultLevelGroup');
    const budgetGroup = document.getElementById('defaultBudgetGroup');
    
    if (mode === 'level') {
        levelGroup.style.display = 'block';
        budgetGroup.style.display = 'none';
    } else if (mode === 'budget') {
        levelGroup.style.display = 'none';
        budgetGroup.style.display = 'block';
    } else {
        levelGroup.style.display = 'none';
        budgetGroup.style.display = 'none';
    }
}

// 更新级别显示
function updateLevelDisplay() {
    const slider = document.getElementById('defaultLevel');
    const value = LEVEL_MAP[parseInt(slider.value)];
    document.getElementById('levelValue').textContent = value;
}

// 切换全局开关
function toggleThinkingEnabled() {
    thinkingConfig.enabled = document.getElementById('thinkingEnabled').checked;
}

// 渲染提供商覆盖
function renderProviderOverrides() {
    const container = document.getElementById('providerOverrides');
    container.innerHTML = '';
    
    const providers = thinkingConfig.providers || {};
    Object.keys(providers).forEach(provider => {
        const override = providers[provider];
        const card = createProviderOverrideCard(provider, override);
        container.appendChild(card);
    });
}

// 创建提供商覆盖卡片
function createProviderOverrideCard(provider, override) {
    const div = document.createElement('div');
    div.className = 'provider-override-card';
    div.dataset.provider = provider;
    
    const mode = override.mode || 'level';
    const level = override.level || 'medium';
    const budget = override.budget || 8192;
    
    div.innerHTML = `
        <label>${provider}:</label>
        <select class="provider-mode" onchange="updateProviderMode('${provider}', this.value)">
            <option value="level" ${mode === 'level' ? 'selected' : ''}>级别模式</option>
            <option value="budget" ${mode === 'budget' ? 'selected' : ''}>预算模式</option>
            <option value="auto" ${mode === 'auto' ? 'selected' : ''}>自动模式</option>
        </select>
        <select class="provider-level" ${mode !== 'level' ? 'style="display:none;"' : ''}>
            ${LEVEL_MAP.map(l => `<option value="${l}" ${l === level ? 'selected' : ''}>${l}</option>`).join('')}
        </select>
        <input type="number" class="provider-budget form-control" value="${budget}" 
               min="512" max="128000" step="512" ${mode !== 'budget' ? 'style="display:none;"' : ''}>
        <button class="action-btn delete" onclick="deleteProviderOverride('${provider}')">🗑️</button>
    `;
    
    return div;
}

// 添加提供商覆盖
function addProviderOverride() {
    const provider = prompt('输入提供商名称 (例如: claude, codex):');
    if (!provider) return;
    
    if (thinkingConfig.providers[provider]) {
        alert('该提供商已存在');
        return;
    }
    
    thinkingConfig.providers[provider] = {
        mode: 'level',
        level: 'medium'
    };
    
    renderProviderOverrides();
}

// 更新提供商模式
function updateProviderMode(provider, mode) {
    const card = document.querySelector(`.provider-override-card[data-provider="${provider}"]`);
    const levelSelect = card.querySelector('.provider-level');
    const budgetInput = card.querySelector('.provider-budget');
    
    if (mode === 'level') {
        levelSelect.style.display = 'block';
        budgetInput.style.display = 'none';
        thinkingConfig.providers[provider] = {
            mode: 'level',
            level: levelSelect.value
        };
    } else if (mode === 'budget') {
        levelSelect.style.display = 'none';
        budgetInput.style.display = 'block';
        thinkingConfig.providers[provider] = {
            mode: 'budget',
            budget: parseInt(budgetInput.value)
        };
    } else {
        levelSelect.style.display = 'none';
        budgetInput.style.display = 'none';
        thinkingConfig.providers[provider] = {
            mode: 'auto'
        };
    }
}

// 删除提供商覆盖
function deleteProviderOverride(provider) {
    if (!confirm(`确定删除 ${provider} 的覆盖配置吗？`)) return;
    delete thinkingConfig.providers[provider];
    renderProviderOverrides();
}

// 渲染模型覆盖
function renderModelOverrides() {
    const tbody = document.getElementById('modelOverridesBody');
    tbody.innerHTML = '';
    
    const models = thinkingConfig.models || {};
    Object.keys(models).forEach(modelName => {
        const override = models[modelName];
        const row = createModelOverrideRow(modelName, override);
        tbody.appendChild(row);
    });
    
    if (Object.keys(models).length === 0) {
        tbody.innerHTML = '<tr><td colspan="4" style="text-align:center;color:var(--text-secondary);">暂无模型级覆盖</td></tr>';
    }
}

// 创建模型覆盖行
function createModelOverrideRow(modelName, override) {
    const tr = document.createElement('tr');
    tr.dataset.model = modelName;
    
    const mode = override.mode || 'level';
    const level = override.level || 'medium';
    const budget = override.budget || 8192;
    
    const displayValue = mode === 'level' ? level : 
                        mode === 'budget' ? `${budget} tokens` :
                        mode === 'auto' ? 'Auto' : 'None';
    
    tr.innerHTML = `
        <td>${modelName}</td>
        <td>${mode === 'level' ? '级别' : mode === 'budget' ? '预算' : mode}</td>
        <td>${displayValue}</td>
        <td>
            <button class="action-btn" onclick="editModelOverride('${modelName}')">✏️</button>
            <button class="action-btn delete" onclick="deleteModelOverride('${modelName}')">🗑️</button>
        </td>
    `;
    
    return tr;
}

// 添加模型覆盖
function addModelOverride() {
    const modelName = prompt('输入模型名称 (例如: claude-opus-4-8):');
    if (!modelName) return;
    
    if (thinkingConfig.models[modelName]) {
        alert('该模型已存在，请直接编辑');
        return;
    }
    
    const mode = prompt('选择模式 (level/budget/auto):', 'level');
    if (!mode) return;
    
    let override = { mode };
    
    if (mode === 'level') {
        const level = prompt('选择级别 (minimal/low/medium/high/xhigh/max):', 'medium');
        if (!level) return;
        override.level = level;
    } else if (mode === 'budget') {
        const budget = prompt('输入预算 (512-128000):', '8192');
        if (!budget) return;
        override.budget = parseInt(budget);
    }
    
    thinkingConfig.models[modelName] = override;
    renderModelOverrides();
}

// 编辑模型覆盖
function editModelOverride(modelName) {
    const current = thinkingConfig.models[modelName];
    const mode = prompt('选择模式 (level/budget/auto):', current.mode || 'level');
    if (!mode) return;
    
    let override = { mode };
    
    if (mode === 'level') {
        const level = prompt('选择级别 (minimal/low/medium/high/xhigh/max):', current.level || 'medium');
        if (!level) return;
        override.level = level;
    } else if (mode === 'budget') {
        const budget = prompt('输入预算 (512-128000):', current.budget || 8192);
        if (!budget) return;
        override.budget = parseInt(budget);
    }
    
    thinkingConfig.models[modelName] = override;
    renderModelOverrides();
}

// 删除模型覆盖
function deleteModelOverride(modelName) {
    if (!confirm(`确定删除 ${modelName} 的覆盖配置吗？`)) return;
    delete thinkingConfig.models[modelName];
    renderModelOverrides();
}

// 保存配置
async function saveThinkingConfig() {
    // 收集表单数据
    thinkingConfig.enabled = document.getElementById('thinkingEnabled').checked;
    thinkingConfig.default_mode = document.getElementById('defaultMode').value;
    
    const mode = thinkingConfig.default_mode;
    if (mode === 'level') {
        const levelIndex = parseInt(document.getElementById('defaultLevel').value);
        thinkingConfig.default_level = LEVEL_MAP[levelIndex];
    } else if (mode === 'budget') {
        thinkingConfig.default_budget = parseInt(document.getElementById('defaultBudget').value);
    }
    
    // 更新提供商覆盖（从 DOM 读取最新值）
    document.querySelectorAll('.provider-override-card').forEach(card => {
        const provider = card.dataset.provider;
        const modeSelect = card.querySelector('.provider-mode');
        const mode = modeSelect.value;
        
        if (mode === 'level') {
            const levelSelect = card.querySelector('.provider-level');
            thinkingConfig.providers[provider] = {
                mode: 'level',
                level: levelSelect.value
            };
        } else if (mode === 'budget') {
            const budgetInput = card.querySelector('.provider-budget');
            thinkingConfig.providers[provider] = {
                mode: 'budget',
                budget: parseInt(budgetInput.value)
            };
        } else {
            thinkingConfig.providers[provider] = { mode };
        }
    });
    
    try {
        const resp = await fetch('/admin/thinking', {
            method: 'POST',
            headers: {
                'Authorization': `Bearer ${getToken()}`,
                'Content-Type': 'application/json'
            },
            body: JSON.stringify(thinkingConfig)
        });
        
        if (!resp.ok) {
            const error = await resp.text();
            throw new Error(error);
        }
        
        showNotification('✅ Thinking 配置已保存', 'success');
    } catch (err) {
        console.error('Failed to save thinking config:', err);
        showNotification('❌ 保存失败: ' + err.message, 'error');
    }
}

// 实时预览
async function updatePreview() {
    const provider = document.getElementById('previewProvider').value;
    const model = document.getElementById('previewModel').value.trim();
    
    if (!model) {
        document.getElementById('previewResult').innerHTML = '<p style="color:var(--text-secondary);">请输入模型名</p>';
        return;
    }
    
    try {
        const resp = await fetch('/admin/thinking/preview', {
            method: 'POST',
            headers: {
                'Authorization': `Bearer ${getToken()}`,
                'Content-Type': 'application/json'
            },
            body: JSON.stringify({
                provider: provider,
                model: model,
                body: {}  // 空请求体用于测试
            })
        });
        
        if (!resp.ok) {
            throw new Error(`HTTP ${resp.status}`);
        }
        
        const result = await resp.json();
        renderPreviewResult(result);
    } catch (err) {
        document.getElementById('previewResult').innerHTML = `<p style="color:var(--danger);">预览失败: ${err.message}</p>`;
    }
}

// 渲染预览结果
function renderPreviewResult(result) {
    const box = document.getElementById('previewResult');
    
    const source = result.source || '全局默认';
    const config = result.resolved_config || {};
    const appliedBody = result.applied_body || {};
    
    let html = '<pre>';
    html += `<span class="preview-label">配置来源:</span> <span class="preview-value">${source}</span>\n\n`;
    html += `<span class="preview-label">解析配置:</span>\n`;
    html += `  Mode: <span class="preview-value">${config.mode || 'N/A'}</span>\n`;
    
    if (config.level) {
        html += `  Level: <span class="preview-value">${config.level}</span>\n`;
    }
    if (config.budget) {
        html += `  Budget: <span class="preview-value">${config.budget} tokens</span>\n`;
    }
    
    html += `\n<span class="preview-label">上游请求体:</span>\n`;
    html += JSON.stringify(appliedBody, null, 2);
    html += '</pre>';
    
    box.innerHTML = html;
}

// 在导航切换时调用
function loadSectionData(section) {
    // ... 现有代码 ...
    
    if (section === 'thinking') {
        loadThinkingConfig();
    }
}

// 通知函数
function showNotification(message, type = 'info') {
    // 简单实现（可以使用更好的 toast 库）
    const notification = document.createElement('div');
    notification.style.cssText = `
        position: fixed;
        top: 20px;
        right: 20px;
        padding: 16px 20px;
        background: ${type === 'success' ? '#10b981' : type === 'error' ? '#ef4444' : '#3b82f6'};
        color: white;
        border-radius: 8px;
        box-shadow: 0 4px 12px rgba(0,0,0,0.15);
        z-index: 10000;
        animation: slideIn 0.3s ease;
    `;
    notification.textContent = message;
    document.body.appendChild(notification);
    
    setTimeout(() => {
        notification.style.opacity = '0';
        notification.style.transform = 'translateX(100%)';
        setTimeout(() => notification.remove(), 300);
    }, 3000);
}

// Helper: 获取 token
function getToken() {
    return localStorage.getItem('admin_token') || '';
}
```

---

## 五、API 端点设计

### 5.1 后端需要实现的接口

```go
// GET /admin/thinking - 获取当前配置
func handleGetThinkingConfig(w http.ResponseWriter, r *http.Request) {
    config := getThinkingConfigFromFile() // 从 config.yaml 读取
    respondJSON(w, config)
}

// POST /admin/thinking - 保存配置
func handleSaveThinkingConfig(w http.ResponseWriter, r *http.Request) {
    var config ThinkingConfig
    if err := json.NewDecoder(r.Body).Decode(&config); err != nil {
        http.Error(w, err.Error(), http.StatusBadRequest)
        return
    }
    
    // 验证配置
    if err := validateThinkingConfig(config); err != nil {
        http.Error(w, err.Error(), http.StatusBadRequest)
        return
    }
    
    // 保存到 config.yaml（或内存热更新）
    if err := saveThinkingConfigToFile(config); err != nil {
        http.Error(w, err.Error(), http.StatusInternalServerError)
        return
    }
    
    respondJSON(w, map[string]string{"status": "ok"})
}

// POST /admin/thinking/preview - 预览配置应用结果
func handlePreviewThinking(w http.ResponseWriter, r *http.Request) {
    var req struct {
        Provider string          `json:"provider"`
        Model    string          `json:"model"`
        Body     json.RawMessage `json:"body"`
    }
    
    if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
        http.Error(w, err.Error(), http.StatusBadRequest)
        return
    }
    
    // 解析配置
    config := thinking.ResolveConfig(globalConfig, req.Provider, req.Model, Account{})
    
    // 应用 thinking
    modelInfo := registry.LookupModelInfo(req.Model, req.Provider)
    appliedBody, err := thinking.ApplyThinking(req.Body, config, modelInfo, req.Provider)
    if err != nil {
        http.Error(w, err.Error(), http.StatusBadRequest)
        return
    }
    
    // 返回结果
    respondJSON(w, map[string]interface{}{
        "source":          determineConfigSource(req.Provider, req.Model),
        "resolved_config": config,
        "applied_body":    json.RawMessage(appliedBody),
    })
}
```

---

## 六、用户体验优化

### 6.1 交互细节

1. **实时反馈**:
   - 滑块拖动时实时显示级别名称
   - 配置保存后显示 toast 通知
   - 预览按钮点击后立即显示 loading 状态

2. **智能默认**:
   - 新增提供商时默认使用 `level: medium`
   - 新增模型时默认继承提供商配置

3. **验证提示**:
   - 预算输入框限制 512-128000 范围
   - 模型名输入时提供自动补全（可选）

4. **视觉提示**:
   - 滑块使用渐变色表示不同级别（绿→黄→红）
   - 启用/禁用开关使用明显的颜色区分
   - 预览框使用暗色主题突出 JSON 内容

### 6.2 移动端适配

```css
@media (max-width: 768px) {
    .provider-override-card {
        grid-template-columns: 1fr;
        gap: 12px;
    }
    
    .preview-controls {
        grid-template-columns: 1fr;
    }
    
    .slider-labels span {
        font-size: 9px;
    }
}
```

---

## 七、完整集成清单

### 7.1 文件修改清单

| 文件 | 修改内容 |
|------|----------|
| `internal/web/assets/index.html` | 添加 thinking 导航项 + section |
| `internal/web/assets/app.css` | 添加 thinking 样式 |
| `internal/web/assets/app.js` | 添加 thinking JS 逻辑 |
| `internal/api/admin.go` | 添加 /admin/thinking* 路由 |
| `internal/api/thinking.go` | **新建**：thinking API handlers |

### 7.2 测试清单

- [ ] 全局开关切换正常
- [ ] 默认模式切换（level/budget/auto）显示正确
- [ ] 滑块拖动实时更新级别名称
- [ ] 提供商覆盖增删改正常
- [ ] 模型覆盖增删改正常
- [ ] 保存配置后立即生效
- [ ] 预览功能正确显示应用结果
- [ ] 移动端响应式布局正常
- [ ] 错误提示清晰易懂

---

## 八、部署注意事项

1. **配置持久化**: 修改后的配置应保存到 `config.yaml` 或专用配置文件
2. **热更新**: 配置变更后无需重启服务即可生效
3. **权限控制**: 只有管理员可以访问 /admin/thinking* 接口
4. **备份机制**: 修改前自动备份当前配置

---

**文档版本**: 1.0  
**创建日期**: 2026-06-09  
**作者**: Claude (Kiro AI)  
**状态**: 前端设计完成
