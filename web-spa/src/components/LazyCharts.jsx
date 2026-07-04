import React, { Suspense, lazy } from 'react';

function lazyChart(exportName) {
  return lazy(() => import('./Charts.jsx').then((module) => ({ default: module[exportName] })));
}

const LazyUsageAreaChart = lazyChart('UsageAreaChart');
const LazyDonutChart = lazyChart('DonutChart');
const LazyGroupedBar = lazyChart('GroupedBar');
const LazyCacheRateBars = lazyChart('CacheRateBars');
const LazyUsageModelAreaChart = lazyChart('UsageModelAreaChart');

function ChartFallback({ height = 240 }) {
  return (
    <div
      style={{
        height,
        minHeight: 120,
        display: 'flex',
        alignItems: 'center',
        justifyContent: 'center',
        color: 'var(--pool-text-2)',
        fontSize: 13,
      }}
    >
      加载图表...
    </div>
  );
}

function withChartFallback(Component, fallbackHeight = 240) {
  return function DeferredChart(props) {
    return (
      <Suspense fallback={<ChartFallback height={props.height || fallbackHeight} />}>
        <Component {...props} />
      </Suspense>
    );
  };
}

export const UsageAreaChart = withChartFallback(LazyUsageAreaChart, 260);
export const DonutChart = withChartFallback(LazyDonutChart, 240);
export const GroupedBar = withChartFallback(LazyGroupedBar, 240);
export const CacheRateBars = withChartFallback(LazyCacheRateBars, 160);
export const UsageModelAreaChart = withChartFallback(LazyUsageModelAreaChart, 260);
