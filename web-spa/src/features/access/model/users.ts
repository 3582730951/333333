export interface UserRow {
  id: string;
  email: string;
  name?: string;
  role: 'admin' | 'user' | string;
  status: 'active' | 'disabled' | string;
  created_at?: number;
  [key: string]: unknown;
}

export interface UserCreateInput {
  email: string;
  name: string;
  role: string;
  status: string;
  password: string;
}

export interface UserUpdateInput {
  id: string;
  values: { name?: string; role?: string; status?: string; password?: string };
}
