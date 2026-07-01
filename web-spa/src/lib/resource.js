export async function loadResourceGroup(resources) {
  const entries = Object.entries(resources);
  const settled = await Promise.allSettled(entries.map(([, resource]) => Promise.resolve().then(() => resource.load())));
  const values = {};
  const failures = [];

  entries.forEach(([key, resource], index) => {
    const result = settled[index];
    if (result.status === 'fulfilled') {
      values[key] = result.value;
      return;
    }
    failures.push({ key, label: resource.label || key, error: result.reason });
  });

  if (!failures.length) return { values, error: null };

  const error = new Error(`部分数据读取失败：${failures.map((f) => f.label).join('、')}`);
  error.failures = failures;
  if (failures.length === entries.length) throw error;

  return { values, error };
}
