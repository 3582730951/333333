export const ArrayExpressionFixture = ({ condition }: { condition: boolean }) => (
  <div className={['pool-fixture-array-missing', condition && 'pool-fixture-array-conditional-missing'].filter(Boolean).join(' ')} />
);
