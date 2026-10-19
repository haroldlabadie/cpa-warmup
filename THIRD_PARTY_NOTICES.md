# 第三方来源

ABI 1 C 声明、RPC envelope 与数据类型参考 key-model-access 的 MIT 实现（本机研究版本 0.5.1；仓库 `https://github.com/haroldlabadie/key-model-access`）。仅提取 ABI/请求/管理结构，未复制权限策略或业务循环。后续宿主升级需重新验证 ABI，不把参考源码版本当宿主兼容承诺。

```text
MIT License

Copyright (c) 2026 Supra4E8C

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
```

额度与调度语义来自 fabcomity 现行 `cpa_quota_guard.py` 的运维规则；实现未复制凭据或现场配置。`gopkg.in/yaml.v3` 使用其原始 MIT/Apache 2.0 许可证。
