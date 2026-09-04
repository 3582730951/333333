export const LogicalExpressionFixture = ({ condition }: { condition: boolean }) => (
  <div className={(condition && 'pool-fixture-logical-missing') || ''} />
);
