package dynamostore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// TTLAttribute is the table's time-to-live attribute (epoch seconds).
const TTLAttribute = "expires"

// CreateTable creates the relay table the way Terraform does in production
// (string pk/sk, on-demand, TTL on "expires"), for development, tests and
// DynamoDB Local. Production tables come from Terraform, which also turns
// on point-in-time recovery and deletion protection.
func CreateTable(ctx context.Context, db *dynamodb.Client, name string) error {
	_, err := db.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName: aws.String(name),
		AttributeDefinitions: []types.AttributeDefinition{
			{AttributeName: aws.String("pk"), AttributeType: types.ScalarAttributeTypeS},
			{AttributeName: aws.String("sk"), AttributeType: types.ScalarAttributeTypeS},
		},
		KeySchema: []types.KeySchemaElement{
			{AttributeName: aws.String("pk"), KeyType: types.KeyTypeHash},
			{AttributeName: aws.String("sk"), KeyType: types.KeyTypeRange},
		},
		BillingMode: types.BillingModePayPerRequest,
	})
	var inUse *types.ResourceInUseException
	if errors.As(err, &inUse) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("dynamostore: create table %s: %w", name, err)
	}
	if err := dynamodb.NewTableExistsWaiter(db).Wait(ctx, &dynamodb.DescribeTableInput{TableName: aws.String(name)},
		2*time.Minute); err != nil {
		return err
	}
	_, err = db.UpdateTimeToLive(ctx, &dynamodb.UpdateTimeToLiveInput{
		TableName: aws.String(name),
		TimeToLiveSpecification: &types.TimeToLiveSpecification{
			AttributeName: aws.String(TTLAttribute), Enabled: aws.Bool(true)},
	})
	return err
}
