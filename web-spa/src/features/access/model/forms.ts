import { z } from 'zod';

export const apiKeyFormSchema = z.object({
  label: z.string().trim().min(1, '请输入 Key 名称').max(80, 'Key 名称不能超过 80 个字符'),
  key_type: z.enum(['downstream', 'pool_import']).default('downstream'),
  group_name: z.string().trim().max(120).default(''),
  force_model: z.string().trim().max(160).default(''),
  force_effort: z.enum(['', 'minimal', 'low', 'medium', 'high', 'xhigh', 'max', 'ultra']).default(''),
  expires_at: z.string().trim().refine((value) => !value || !Number.isNaN(Date.parse(value)), '过期时间格式无效').default(''),
});

export const userFormSchema = z.object({
  email: z.email('请输入有效邮箱'),
  name: z.string().trim().max(120).default(''),
  role: z.enum(['user', 'admin']).default('user'),
  status: z.enum(['active', 'disabled']).default('active'),
  password: z.string().refine((value) => !value || value.length >= 8, '密码至少需要 8 位').default(''),
});

export const accountImportSchema = z.object({
  provider: z.string().trim().min(1, '请选择服务商'),
  label: z.string().trim().max(120).default(''),
  credential: z.string().trim().min(1, '请输入账号凭据'),
  group_name: z.string().trim().max(120).default(''),
});

export type ApiKeyForm = z.infer<typeof apiKeyFormSchema>;
export type UserForm = z.infer<typeof userFormSchema>;
export type AccountImportForm = z.infer<typeof accountImportSchema>;
