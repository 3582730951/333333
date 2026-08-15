import type {
  ButtonHTMLAttributes,
  ComponentType,
  CSSProperties,
  FormHTMLAttributes,
  ForwardRefExoticComponent,
  HTMLAttributes,
  InputHTMLAttributes,
  Key,
  ReactElement,
  ReactNode,
  RefAttributes,
  TextareaHTMLAttributes,
} from 'react';

export type ButtonTheme = 'solid' | 'borderless' | 'light' | 'outline';
export type ButtonIntent = 'primary' | 'danger' | 'tertiary';
export type ButtonSize = 'small' | 'large';

export interface ButtonProps extends Omit<ButtonHTMLAttributes<HTMLButtonElement>, 'type'> {
  icon?: ReactNode;
  loading?: boolean;
  theme?: ButtonTheme;
  type?: ButtonIntent;
  size?: ButtonSize;
  block?: boolean;
  htmlType?: 'button' | 'submit' | 'reset';
}

export const Button: ForwardRefExoticComponent<ButtonProps & RefAttributes<HTMLButtonElement>>;
export interface IconButtonProps extends ButtonProps {
  icon: ReactNode;
  label?: string;
}
export const IconButton: ComponentType<IconButtonProps>;

export interface TableColumn<Row = Record<string, unknown>> {
  key?: Key;
  title?: ReactNode;
  dataIndex?: keyof Row | string;
  width?: number | string;
  align?: 'left' | 'center' | 'right';
  children?: ReadonlyArray<TableColumn<Row>>;
  render?: (value: unknown, row: Row, index: number) => ReactNode;
  sorter?: (left: Row, right: Row) => number;
  defaultSortOrder?: 'ascend' | 'descend';
}

export interface TablePagination {
  currentPage?: number;
  pageSize?: number;
  total?: number;
  onPageChange?: (page: number) => void;
}

export interface TableRowSelection<Row = Record<string, unknown>> {
  selectedRowKeys?: ReadonlyArray<Key>;
  onChange?: (keys: Key[]) => void;
  getCheckboxProps?: (row: Row) => { disabled?: boolean; 'aria-label'?: string };
}

export interface DataTableProps<Row = Record<string, unknown>> {
  dataSource?: ReadonlyArray<Row>;
  columns?: ReadonlyArray<TableColumn<Row>>;
  rowKey?: keyof Row | string | ((row: Row, index: number) => Key);
  pagination?: false | TablePagination;
  empty?: ReactNode;
  loading?: boolean;
  rowSelection?: TableRowSelection<Row>;
  className?: string;
  style?: CSSProperties;
  scroll?: false | { x?: number | string; y?: number | string };
  onRow?: (row: Row, index: number) => HTMLAttributes<HTMLTableRowElement>;
  expandedRowRender?: (row: Row, index: number) => ReactNode;
  'aria-label'?: string;
  [prop: string]: unknown;
}

export function DataTable<Row = Record<string, unknown>>(props: DataTableProps<Row>): ReactElement;
export const Table: typeof DataTable;

export interface ModalProps extends Omit<HTMLAttributes<HTMLDivElement>, 'title'> {
  open?: boolean;
  visible?: boolean;
  onOpenChange?: (open: boolean) => void;
  onCancel?: () => void;
  onClose?: () => void;
  onOk?: () => void;
  confirmLoading?: boolean;
  okText?: ReactNode;
  cancelText?: ReactNode;
  title?: ReactNode;
  description?: ReactNode;
  footer?: ReactNode | null;
  width?: number | string;
  maskClosable?: boolean;
}

export const Modal: ComponentType<ModalProps>;
export interface DrawerProps extends Omit<ModalProps, 'onOk' | 'confirmLoading' | 'okText' | 'cancelText' | 'maskClosable'> {}
export const Drawer: ComponentType<DrawerProps>;
export interface ConfirmDialogProps {
  open?: boolean;
  title: ReactNode;
  description?: ReactNode;
  confirmText?: ReactNode;
  cancelText?: ReactNode;
  destructive?: boolean;
  busy?: boolean;
  onConfirm?: () => void | Promise<void>;
  onCancel?: () => void;
  children?: ReactNode;
}
export const ConfirmDialog: ComponentType<ConfirmDialogProps>;

export interface FormRule {
  required?: boolean;
  type?: 'email';
  min?: number;
  max?: number;
  pattern?: RegExp;
  message?: string;
}

export interface FieldProps {
  field?: string;
  label?: ReactNode;
  help?: ReactNode;
  rules?: ReadonlyArray<FormRule>;
  initValue?: unknown;
}

export interface TextInputProps extends FieldProps, Omit<InputHTMLAttributes<HTMLInputElement>, 'onChange' | 'value' | 'prefix' | 'size'> {
  value?: unknown;
  onChange?: (...args: any[]) => void;
  onEnterPress?: (event: React.KeyboardEvent<HTMLInputElement>) => void;
  prefix?: ReactNode;
  showClear?: boolean;
  allowReveal?: boolean;
  onClear?: () => void;
  mode?: 'text' | 'password';
}

export interface TextareaProps extends FieldProps, Omit<TextareaHTMLAttributes<HTMLTextAreaElement>, 'onChange' | 'value'> {
  value?: unknown;
  onChange?: (...args: any[]) => void;
  autosize?: boolean;
}

export interface FormProps extends Omit<FormHTMLAttributes<HTMLFormElement>, 'onSubmit' | 'onChange'> {
  initValues?: Record<string, unknown>;
  onSubmit?: (values: any) => unknown | Promise<unknown>;
  onValueChange?: (values: Record<string, unknown>, changedValues: Record<string, unknown>) => void;
  onChange?: (...args: any[]) => void;
  labelPosition?: 'top' | 'left';
  labelWidth?: number | string;
  layout?: string;
  disabled?: boolean;
  getFormApi?: (api: any) => void;
}

type FormComponent = ComponentType<FormProps> & {
  Input: ComponentType<TextInputProps>;
  InputNumber: ComponentType<FieldProps & Record<string, unknown>>;
  TextArea: ComponentType<TextareaProps>;
  Select: ComponentType<FieldProps & Record<string, unknown>>;
  Switch: ComponentType<FieldProps & Record<string, unknown>>;
  Slot: ComponentType<FieldProps & { children?: ReactNode }>;
};

export const Form: FormComponent;
export const Input: ComponentType<TextInputProps>;
export const TextInput: ComponentType<TextInputProps>;
export const Textarea: ComponentType<TextareaProps>;
export const InputNumber: ComponentType<FieldProps & Record<string, unknown>>;
export const Select: ComponentType<FieldProps & Record<string, unknown>>;
export const SelectInput: ComponentType<FieldProps & Record<string, unknown>>;
export const Switch: ComponentType<FieldProps & Record<string, unknown>>;
export const Toggle: ComponentType<FieldProps & Record<string, unknown>>;

export interface ToastAPI {
  success(message: ReactNode): void;
  error(message: ReactNode): void;
  warning(message: ReactNode): void;
  info(message: ReactNode): void;
}
export const Toast: ToastAPI;
export const ToastViewport: ComponentType;

export interface NavItem {
  itemKey: string;
  text: string;
  icon?: ReactNode;
  items?: ReadonlyArray<NavItem>;
}
export interface NavProps {
  items?: ReadonlyArray<NavItem>;
  selectedKeys?: ReadonlyArray<string>;
  isCollapsed?: boolean;
  onClick?: (event: { itemKey: string; group?: boolean }) => void;
  className?: string;
  style?: CSSProperties;
}
export const Nav: ComponentType<NavProps>;

type LayoutPart = ComponentType<HTMLAttributes<HTMLElement>>;
export const Layout: ComponentType<HTMLAttributes<HTMLDivElement>> & {
  Header: LayoutPart;
  Sider: LayoutPart;
  Content: LayoutPart;
};

// These smaller primitives keep their implementation-driven surface while the
// high-risk interactive contracts above remain fully typed.
export const Card: ComponentType<any>;
export const DataCard: ComponentType<any>;
export const MetricCard: ComponentType<any>;
export const ActionMenu: ComponentType<any>;
export const EmptyState: ComponentType<any>;
export const LoadingState: ComponentType<any>;
export const ErrorState: ComponentType<any>;
export const Avatar: ComponentType<any>;
export const Banner: ComponentType<any>;
export const Divider: ComponentType<any>;
export const LocaleProvider: ComponentType<any>;
export const Space: ComponentType<any>;
export const Spin: ComponentType<any>;
export const Tag: ComponentType<any>;
export const Tooltip: ComponentType<any>;
export const Typography: {
  Text: ComponentType<any>;
  Title: ComponentType<any>;
};
export const Progress: ComponentType<any>;
export const ProgressBar: ComponentType<any>;
export const StatusDot: ComponentType<any>;
export const StatusPill: ComponentType<any>;
export const Tabs: ComponentType<any>;
export const TabPane: ComponentType<any>;
