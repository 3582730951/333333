export const ConditionalExpressionFixture = ({ condition }: { condition: boolean }) => (
  <div className={condition ? 'pool-fixture-conditional-a-missing' : 'pool-fixture-conditional-b-missing'} />
);
