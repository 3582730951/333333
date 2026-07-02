import React from 'react';
import {
  Activity,
  BarChart3,
  Check,
  CheckCircle2,
  ChevronDown,
  ChevronRight,
  Copy,
  Download,
  Edit3,
  FileText,
  Globe2,
  Home,
  Inbox,
  KeyRound,
  Languages,
  Link,
  List,
  LogOut,
  Moon,
  MoreHorizontal,
  Play,
  Plus,
  RefreshCw,
  Save,
  Search,
  Settings,
  Sun,
  Trash2,
  Undo2,
  User,
  Users,
  X,
  LineChart,
} from 'lucide-react';

function icon(Component) {
  return function PoolIcon(props) {
    return <Component className="pool-icon" strokeWidth={1.8} {...props} />;
  };
}

export const IconCheckCircleStroked = icon(CheckCircle2);
export const IconChevronDown = icon(ChevronDown);
export const IconChevronRight = icon(ChevronRight);
export const IconClose = icon(X);
export const IconCopy = icon(Copy);
export const IconDelete = icon(Trash2);
export const IconDownload = icon(Download);
export const IconEdit = icon(Edit3);
export const IconExit = icon(LogOut);
export const IconFile = icon(FileText);
export const IconGlobe = icon(Globe2);
export const IconHistogram = icon(BarChart3);
export const IconHome = icon(Home);
export const IconInbox = icon(Inbox);
export const IconKey = icon(KeyRound);
export const IconLanguage = icon(Languages);
export const IconLineChartStroked = icon(LineChart);
export const IconLink = icon(Link);
export const IconList = icon(List);
export const IconMoon = icon(Moon);
export const IconPlay = icon(Play);
export const IconPlus = icon(Plus);
export const IconPulse = icon(Activity);
export const IconRefresh = icon(RefreshCw);
export const IconSave = icon(Save);
export const IconSearch = icon(Search);
export const IconSetting = icon(Settings);
export const IconSun = icon(Sun);
export const IconTick = icon(Check);
export const IconUndo = icon(Undo2);
export const IconUser = icon(User);
export const IconUserGroup = icon(Users);

export {
  Check,
  ChevronDown,
  ChevronRight,
  Copy,
  Download,
  Edit3,
  FileText,
  Globe2,
  Home,
  Inbox,
  KeyRound,
  Languages,
  Link,
  List,
  LogOut,
  MoreHorizontal,
  Play,
  Plus,
  RefreshCw,
  Save,
  Search,
  Settings,
  Sun,
  Moon,
  Trash2,
  X,
};
