declare const clsx: (...values: unknown[]) => string;

export const ClsxCallFixture = ({ condition }: { condition: boolean }) => (
  <div className={clsx('pool-fixture-clsx-missing', condition && 'pool-fixture-clsx-logical-missing', { 'pool-fixture-clsx-object-missing': condition })} />
);
