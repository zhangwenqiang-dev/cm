package connectmac

import (
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

func TestValidateReleaseHostOutput(t *testing.T) {
	tests := []struct {
		name string
		out  *ec2.ReleaseHostsOutput
		want string
	}{
		{name: "successful", out: &ec2.ReleaseHostsOutput{Successful: []string{"h-1"}}},
		{
			name: "occupied",
			out: &ec2.ReleaseHostsOutput{Unsuccessful: []ec2types.UnsuccessfulItem{{
				ResourceId: aws.String("h-1"),
				Error: &ec2types.UnsuccessfulItemError{
					Code:    aws.String("Client.HostNotReleasable"),
					Message: aws.String("host is occupied"),
				},
			}}},
			want: "Client.HostNotReleasable: host is occupied",
		},
		{name: "empty", out: &ec2.ReleaseHostsOutput{}, want: "did not confirm"},
		{
			name: "contradictory",
			out: &ec2.ReleaseHostsOutput{
				Successful:   []string{"h-1"},
				Unsuccessful: []ec2types.UnsuccessfulItem{{ResourceId: aws.String("h-1")}},
			},
			want: "contradictory",
		},
		{name: "nil", want: "did not confirm"},
		{name: "different successful host", out: &ec2.ReleaseHostsOutput{Successful: []string{"h-other"}}, want: "did not confirm"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateReleaseHostOutput("h-1", test.out)
			if test.want == "" {
				if err != nil {
					t.Fatalf("validateReleaseHostOutput() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validateReleaseHostOutput() error = %v, want containing %q", err, test.want)
			}
		})
	}
}
